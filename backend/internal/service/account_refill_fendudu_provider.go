package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const refillProviderFendudu = "fendudu"
const fenduduProviderURL = "https://gpt.20251231.xyz/open-api/revoke/my"

type FenduduProduct struct {
	Product       string `json:"product"`
	Label         string `json:"label"`
	AvailableNow  int    `json:"available_now"`
	BlockedReason string `json:"blocked_reason"`
	Expired       bool   `json:"expired"`
}

type fenduduOrder struct {
	Success        bool     `json:"success"`
	Created        int      `json:"created"`
	CycleIDs       []string `json:"cycle_ids"`
	Pending        bool     `json:"pending"`
	Error          string   `json:"error"`
	Warning        string   `json:"warning"`
	successPresent bool
}

func (o *fenduduOrder) UnmarshalJSON(data []byte) error {
	type fields fenduduOrder
	var decoded fields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var envelope struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	*o = fenduduOrder(decoded)
	o.successPresent = envelope.Success != nil
	return nil
}

func (o *fenduduOrder) rejected() bool {
	return o != nil && o.successPresent && !o.Success && !o.Pending &&
		o.Created == 0 && len(o.CycleIDs) == 0 && o.Error != ""
}

var fenduduAPIKeyPattern = regexp.MustCompile(`(?i)rvak_[a-z0-9_-]+`)

func fenduduPublicMessage(message, token string) string {
	// Redact before truncation so a long response cannot expose a key prefix.
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	message = fenduduAPIKeyPattern.ReplaceAllString(message, "[REDACTED]")
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	return string(runes[:refillMin(len(runes), 1024)])
}

type fenduduHTTPError struct {
	remote  *refillProviderError
	message string
}

func (e *fenduduHTTPError) Error() string {
	return fmt.Sprintf("粉嘟嘟 HTTP %d：%s", e.remote.code, e.message)
}

func (e *fenduduHTTPError) Unwrap() error { return e.remote }

type fenduduCycle struct {
	ID           string `json:"id"`
	Product      string `json:"product"`
	Status       string `json:"status"`
	Pushed       bool   `json:"pushed"`
	PushProvider string `json:"push_provider"`
	PushConfigID string `json:"push_config_id"`
	AccountID    string `json:"sub2api_account_id"`
	Email        string `json:"account_email"`
	CreatedAt    string `json:"created_at"`
	PushedAt     string `json:"pushed_at"`
	RevokeAt     string `json:"revoke_at"`
	DeadlineAt   string `json:"deadline_at"`
}

func (c fenduduCycle) expiresAt() *int64 {
	// revoke_at is the supplier's real lease. deadline_at is a fallback only.
	for _, value := range []string{c.RevokeAt, c.DeadlineAt} {
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil && at.Unix() > 0 && at.Unix() <= 253402214399 {
			v := at.Unix()
			return &v
		}
	}
	return nil
}

type fenduduProvider interface {
	Products(context.Context, string) ([]FenduduProduct, error)
	Order(context.Context, string, AccountRefillJob) (*fenduduOrder, error)
	Cycle(context.Context, string, string) (*fenduduCycle, error)
}

type fenduduCyclePage struct {
	Success    bool           `json:"success"`
	Items      []fenduduCycle `json:"items"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor"`
}

// Recovery only reads existing orders; it never retries the purchasing POST.
type fenduduCycleReader interface {
	Cycles(context.Context, string, string) (*fenduduCyclePage, error)
}

type httpFenduduProvider struct {
	client *http.Client
	base   string
}

func newHTTPFenduduProvider() *httpFenduduProvider {
	return &httpFenduduProvider{base: fenduduProviderURL, client: &http.Client{
		Timeout:       25 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// New orders carry their durable task ID as the supplier idempotency key.
// Transport errors still never replay purchasing requests automatically.
func (p *httpFenduduProvider) request(ctx context.Context, token, method, path string, body, out any) error {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(raw))
	if err != nil {
		return errors.New("粉嘟嘟请求构建失败")
	}
	req.Header.Set("X-Api-Key", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return &refillProviderTemporaryError{"粉嘟嘟连接中断，需核对原任务"}
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	complete := readErr == nil && len(data) <= 2*1024*1024
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := 15 * time.Second
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(refillMin(seconds, 86400)) * time.Second
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			delay = refillMax(delay, time.Until(at))
		}
		remote := &refillProviderError{code: resp.StatusCode, retryAfter: delay}
		var failure struct {
			Error string `json:"error"`
		}
		if complete && json.Unmarshal(data, &failure) == nil {
			// Preserve a complete rejection envelope for the worker. A failed
			// or partial decode must never release an uncertain order's budget.
			if order, ok := out.(*fenduduOrder); ok {
				var decoded fenduduOrder
				if json.Unmarshal(data, &decoded) == nil {
					*order = decoded
				}
			}
			if message := fenduduPublicMessage(failure.Error, token); message != "" {
				return &fenduduHTTPError{remote: remote, message: message}
			}
		}
		return remote
	}
	if !complete || json.Unmarshal(data, out) != nil {
		return &refillProviderTemporaryError{"粉嘟嘟响应不完整，需核对原任务"}
	}
	return nil
}

func (p *httpFenduduProvider) Products(ctx context.Context, token string) ([]FenduduProduct, error) {
	var out struct {
		Success  bool             `json:"success"`
		Products []FenduduProduct `json:"products"`
	}
	if err := p.request(ctx, token, http.MethodGet, "/products", nil, &out); err != nil {
		return nil, err
	}
	if !out.Success || len(out.Products) == 0 {
		return nil, errors.New("粉嘟嘟未返回有效产品次数，请检查对接码权限")
	}
	seen := map[string]bool{}
	for i := range out.Products {
		v := &out.Products[i]
		if v.Product == "" || seen[v.Product] || v.AvailableNow < 0 {
			return nil, errors.New("粉嘟嘟产品次数响应无效")
		}
		seen[v.Product] = true
		// Never expose provider text that could reflect an authentication value.
		v.Label = v.Product
		if strings.TrimSpace(v.BlockedReason) != "" {
			v.BlockedReason = "该产品当前不可下单，请在粉嘟嘟核对次数、时段和每日限额"
		}
	}
	return out.Products, nil
}

func (p *httpFenduduProvider) Order(ctx context.Context, token string, j AccountRefillJob) (*fenduduOrder, error) {
	if len(j.ID) < 16 || !validFenduduIdentifier(j.ID, false) {
		return nil, errors.New("粉嘟嘟下单请求编号无效，未发送下单请求")
	}
	groups := make([]string, len(j.DeliveryGroupIDs))
	for i, id := range j.DeliveryGroupIDs {
		groups[i] = strconv.FormatInt(id, 10)
	}
	// Local delivery policy is applied to the exact pushed account. Disable
	// supplier subscription automation, which could otherwise toggle scheduling.
	body := map[string]any{"product": j.Plan, "count": 1, "request_id": j.ID, "sub2api_config_id": j.FenduduConnectionID,
		"sub2api_group_ids": strings.Join(groups, ","), "subscription_schedule": 0, "push_fallback_config_id": ""}
	var out fenduduOrder
	err := p.request(ctx, token, http.MethodPost, "/order", body, &out)
	out.Error = fenduduPublicMessage(out.Error, token)
	out.Warning = fenduduPublicMessage(out.Warning, token)
	return &out, err
}

func (p *httpFenduduProvider) Cycle(ctx context.Context, token, id string) (*fenduduCycle, error) {
	var out struct {
		Success bool         `json:"success"`
		Cycle   fenduduCycle `json:"cycle"`
	}
	if err := p.request(ctx, token, http.MethodGet, "/cycle-detail?id="+url.QueryEscape(id), nil, &out); err != nil {
		return nil, err
	}
	if !out.Success || out.Cycle.ID != id {
		return nil, errors.New("粉嘟嘟未返回匹配的原订单")
	}
	return &out.Cycle, nil
}

func (p *httpFenduduProvider) Cycles(ctx context.Context, token, cursor string) (*fenduduCyclePage, error) {
	var out fenduduCyclePage
	if err := p.request(ctx, token, http.MethodGet, "/cycles?limit=200&cursor="+url.QueryEscape(cursor), nil, &out); err != nil {
		return nil, err
	}
	if !out.Success || out.Items == nil || len(out.Items) > 200 || (out.HasMore && (out.NextCursor == "" || out.NextCursor == cursor)) {
		return nil, errors.New("粉嘟嘟订单列表响应不完整，继续保留原任务")
	}
	return &out, nil
}
