package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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
	Success  bool     `json:"success"`
	Created  int      `json:"created"`
	CycleIDs []string `json:"cycle_ids"`
}

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
	RevokeAt     string `json:"revoke_at"`
	DeadlineAt   string `json:"deadline_at"`
}

func (c fenduduCycle) expiresAt() *int64 {
	// revoke_at is the supplier's real lease. deadline_at is a fallback only.
	value := c.RevokeAt
	if value == "" {
		value = c.DeadlineAt
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil && at.Unix() > 0 {
		v := at.Unix()
		return &v
	}
	return nil
}

type fenduduProvider interface {
	Products(context.Context, string) ([]FenduduProduct, error)
	Order(context.Context, string, AccountRefillJob) (*fenduduOrder, error)
	Cycle(context.Context, string, string) (*fenduduCycle, error)
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

// The documented API has no idempotency guarantee. Order is called at most
// once after a durable submission marker; HTTP/transport errors never replay it.
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := 15 * time.Second
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(refillMin(seconds, 86400)) * time.Second
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			delay = refillMax(delay, time.Until(at))
		}
		return &refillProviderError{code: resp.StatusCode, retryAfter: delay}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 || json.Unmarshal(data, out) != nil {
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
	groups := make([]string, len(j.DeliveryGroupIDs))
	for i, id := range j.DeliveryGroupIDs {
		groups[i] = strconv.FormatInt(id, 10)
	}
	// Local delivery policy is applied to the exact pushed account. Disable
	// supplier subscription automation, which could otherwise toggle scheduling.
	body := map[string]any{"product": j.Plan, "count": 1, "sub2api_config_id": j.FenduduConnectionID,
		"sub2api_group_ids": strings.Join(groups, ","), "subscription_schedule": 0, "push_fallback_config_id": ""}
	var out fenduduOrder
	err := p.request(ctx, token, http.MethodPost, "/order", body, &out)
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
