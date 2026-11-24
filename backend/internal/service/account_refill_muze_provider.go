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

	"github.com/google/uuid"
)

const refillProviderMuze = "muze"
const muzeProviderURL = "https://shop.supacoder.top/api/my"

// AvailableNow accounts for both prepaid quota and currently allocatable stock.
type MuzeProduct struct {
	Product       string `json:"product"`
	Label         string `json:"label"`
	AvailableNow  int    `json:"available_now"`
	Remaining     int    `json:"remaining"`
	Stock         int    `json:"stock"`
	BlockedReason string `json:"blocked_reason"`
	Expired       bool   `json:"expired"`
}

type muzeOrder struct {
	ID        string
	ExpiresAt *int64
}

type muzeCycle struct {
	ID, RequestID, Product, Status, RefundPhase string
	Pushed                                      bool
	PushProvider, PushConfigID                  string
	AccountID, Subject, Email                   string // Sub2 row ID (if provided) and separate OpenAI account identity.
	CreatedAt, PushedAt, RefundDueAt            string
}

func (c muzeCycle) expiresAt() *int64 {
	at, err := time.Parse(time.RFC3339Nano, c.RefundDueAt)
	if err != nil || at.Unix() <= 0 || at.Unix() > 253402214399 {
		return nil
	}
	unix := at.Unix()
	return &unix
}
func (c muzeCycle) ended() bool {
	return c.RefundPhase == "free" || c.RefundPhase == "returned" || c.Status == "refundedDone" || c.Status == "returned" || c.Status == "closed"
}

// Order detail can include OAuth credentials. Decode only the identity needed to
// verify the account already pushed to Sub2; never retain or log token fields.
type muzeWireOrder struct {
	ID, RequestID, Product, DisplayStage, RefundPhase, PushStatus                  string
	ConnectionID, PushProvider, At, CreatedAt, PushedAt, RefundDueAt, AccountEmail string
	Sub2APIAccountID                                                               json.Number `json:"sub2apiAccountId"`
	RequestIndex, RequestCount                                                     int
	OAuth                                                                          *struct {
		AccountID string `json:"accountId"`
	} `json:"oauth"`
}

func (w muzeWireOrder) singleOrder() bool {
	// Older receipts can omit the index and count. A reported batch must have one item.
	return (w.RequestIndex == 0 || w.RequestIndex == 1) && (w.RequestCount == 0 || w.RequestCount == 1)
}

func (w muzeWireOrder) cycle() muzeCycle {
	at := w.At
	if at == "" {
		at = w.CreatedAt
	}
	c := muzeCycle{ID: w.ID, RequestID: w.RequestID, Product: w.Product, Status: w.DisplayStage, RefundPhase: w.RefundPhase, Pushed: w.PushStatus == "pushed", PushProvider: w.PushProvider, PushConfigID: w.ConnectionID, AccountID: w.Sub2APIAccountID.String(), Email: w.AccountEmail, CreatedAt: at, PushedAt: w.PushedAt, RefundDueAt: w.RefundDueAt}
	if w.OAuth != nil {
		c.Subject = w.OAuth.AccountID
	}
	return c
}

type muzeProvider interface {
	ResolveConnection(context.Context, string, string) (string, error)
	Products(context.Context, string) ([]MuzeProduct, error)
	Order(context.Context, string, AccountRefillJob) (*muzeOrder, error)
	Cycle(context.Context, string, string) (*muzeCycle, error)
}
type muzeOrderFinder interface {
	FindOrder(context.Context, string, AccountRefillJob) (*muzeCycle, error)
}
type httpMuzeProvider struct {
	client *http.Client
	base   string
}

func newHTTPMuzeProvider() *httpMuzeProvider {
	return &httpMuzeProvider{base: muzeProviderURL, client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var muzeTokenPattern = regexp.MustCompile(`^tk_[A-Fa-f0-9]{64}$`)
var muzeSecretInText = regexp.MustCompile(`(?i)tk_[a-z0-9_-]+`)

func muzePublicMessage(message, token string) string {
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	message = muzeSecretInText.ReplaceAllString(message, "[REDACTED]")
	words := []rune(strings.Join(strings.Fields(message), " "))
	return string(words[:min(len(words), 256)])
}

type muzeHTTPError struct {
	remote        *refillProviderError
	code, message string
	rejection     bool
}

func (e *muzeHTTPError) Error() string {
	return fmt.Sprintf("muze HTTP %d：%s", e.remote.code, e.message)
}
func (e *muzeHTTPError) Unwrap() error { return e.remote }
func muzeDefiniteRejection(code string) bool {
	switch code {
	case "quota_unavailable", "stock_unavailable", "product_unavailable", "insufficient_quota", "quota_insufficient", "insufficient_stock", "stock_insufficient", "product_closed", "product_expired", "order_locked", "count_invalid", "connection_invalid", "connection_required", "connection_unavailable", "request_id_invalid", "request_id_required", "unauthorized", "forbidden":
		return true
	}
	return false
}
func (p *httpMuzeProvider) request(ctx context.Context, token, method, path string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("muze 请求构建失败")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return &refillProviderTemporaryError{"muze 请求中断，请核对原订单"}
	}
	defer func() { _ = resp.Body.Close() }()
	const limit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	complete := err == nil && len(raw) <= limit
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := 15 * time.Second
		if n, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && n > 0 {
			delay = time.Duration(min(n, 86400)) * time.Second
		} else if at, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			if d := time.Until(at); d > delay {
				delay = d
			}
		}
		re := &refillProviderError{code: resp.StatusCode, retryAfter: delay}
		var fail struct {
			Code, Error, RequestID string
			Pending, Accepted      bool
			Order                  json.RawMessage
			Orders                 json.RawMessage
		}
		if !complete || json.Unmarshal(raw, &fail) != nil || fail.Error == "" {
			return re
		}
		// A conflict or pending outcome can still represent an accepted purchase.
		reject := resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 && resp.StatusCode != 409 && resp.StatusCode != 408 && muzeDefiniteRejection(fail.Code) && !fail.Pending && !fail.Accepted && len(fail.Order) == 0 && len(fail.Orders) == 0
		return &muzeHTTPError{remote: re, code: fail.Code, message: muzePublicMessage(fail.Error, token), rejection: reject}
	}
	if !complete || json.Unmarshal(raw, out) != nil {
		return &refillProviderTemporaryError{"muze 响应不完整，请核对原订单"}
	}
	return nil
}

func (p *httpMuzeProvider) Products(ctx context.Context, token string) ([]MuzeProduct, error) {
	var data struct {
		Products []struct {
			ID, Label, Mode, BlockedReason string
			Remaining, CardStock           *int
			Closed, Expired                bool
		}
	}
	if err := p.request(ctx, token, http.MethodGet, "/products", nil, &data); err != nil {
		return nil, err
	}
	if data.Products == nil {
		return nil, errors.New("muze 未返回产品配额")
	}
	var products []MuzeProduct
	seen := map[string]bool{}
	for _, r := range data.Products {
		if (r.ID != "pro20x" && r.ID != "pro5x") || (r.Mode != "" && r.Mode != "standard") {
			continue
		}
		if seen[r.ID] || r.Remaining == nil || r.CardStock == nil || *r.Remaining < 0 || *r.CardStock < 0 {
			return nil, errors.New("muze 商品配额或库存无效")
		}
		seen[r.ID] = true
		item := MuzeProduct{Product: r.ID, Label: r.Label, Remaining: *r.Remaining, Stock: *r.CardStock, Expired: r.Expired, AvailableNow: min(*r.Remaining, *r.CardStock)}
		switch {
		case r.Closed || r.Expired || r.BlockedReason != "":
			item.BlockedReason = "该套餐当前暂停接单"
		case item.Stock == 0:
			item.BlockedReason = "该套餐当前无库存"
		case item.Remaining == 0:
			item.BlockedReason = "该套餐次数已用完"
		}
		if item.BlockedReason != "" {
			item.AvailableNow = 0
		}
		products = append(products, item)
	}
	if len(products) == 0 {
		return nil, errors.New("muze 未提供普通 Pro5x / Pro20x 套餐")
	}
	return products, nil
}
func (p *httpMuzeProvider) ResolveConnection(ctx context.Context, token, id string) (string, error) {
	var data struct {
		Connections []struct {
			ID, Provider       string
			Enabled, IsDefault bool
		}
	}
	if err := p.request(ctx, token, http.MethodGet, "/connections", nil, &data); err != nil {
		return "", err
	}
	found := ""
	for _, c := range data.Connections {
		if (id != "" && c.ID != id) || (id == "" && !c.IsDefault) {
			continue
		}
		if !c.Enabled || c.Provider != "sub2api" || !validMuzeIdentifier(c.ID, false) || found != "" {
			return "", errors.New("muze 推送连接须为唯一启用的 Sub2API 连接")
		}
		found = c.ID
	}
	if found == "" {
		return "", errors.New("muze 未找到已启用的 Sub2API 推送连接")
	}
	return found, nil
}
func (p *httpMuzeProvider) Order(ctx context.Context, token string, j AccountRefillJob) (*muzeOrder, error) {
	if _, err := uuid.Parse(j.ID); err != nil || (j.Plan != "pro5x" && j.Plan != "pro20x") || !validMuzeIdentifier(j.MuzeConnectionID, false) {
		return nil, errors.New("muze 下单参数无效")
	}
	body := struct {
		RequestID    string  `json:"requestId"`
		Product      string  `json:"product"`
		Count        int     `json:"count"`
		ConnectionID string  `json:"connectionId"`
		Groups       []int64 `json:"groups"`
	}{j.ID, j.Plan, 1, j.MuzeConnectionID, j.DeliveryGroupIDs}
	var reply struct {
		RequestID string
		Orders    []muzeWireOrder
		Order     *muzeWireOrder
	}
	if err := p.request(ctx, token, http.MethodPost, "/orders", body, &reply); err != nil {
		return nil, err
	}
	if reply.Orders == nil && reply.Order != nil {
		reply.Orders = []muzeWireOrder{*reply.Order}
	}
	if len(reply.Orders) != 1 {
		return nil, errors.New("muze 未确认唯一订单")
	}
	r := reply.Orders[0]
	if !validMuzeIdentifier(r.ID, false) || (reply.Order != nil && reply.Order.ID != r.ID) || (reply.RequestID != "" && reply.RequestID != j.ID) || (r.RequestID != "" && r.RequestID != j.ID) || (reply.RequestID == "" && r.RequestID == "") || (r.Product != "" && r.Product != j.Plan) || !r.singleOrder() {
		return nil, errors.New("muze 回执与本次购买不匹配")
	}
	return &muzeOrder{ID: r.ID, ExpiresAt: r.cycle().expiresAt()}, nil
}
func (p *httpMuzeProvider) Cycle(ctx context.Context, token, id string) (*muzeCycle, error) {
	if !validMuzeIdentifier(id, false) {
		return nil, errors.New("muze 订单号无效")
	}
	var out struct{ Order *muzeWireOrder }
	if err := p.request(ctx, token, http.MethodGet, "/orders/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	if out.Order == nil || out.Order.ID != id || !out.Order.singleOrder() {
		return nil, errors.New("muze 未返回匹配的原订单")
	}
	c := out.Order.cycle()
	return &c, nil
}
func (p *httpMuzeProvider) FindOrder(ctx context.Context, token string, j AccountRefillJob) (*muzeCycle, error) {
	var out struct{ Orders []muzeWireOrder }
	if err := p.request(ctx, token, http.MethodGet, "/orders", nil, &out); err != nil {
		return nil, err
	}
	var id string
	for _, o := range out.Orders {
		if o.RequestID != j.ID {
			continue
		}
		if id != "" || !validMuzeIdentifier(o.ID, false) || o.Product != j.Plan || !o.singleOrder() {
			return nil, errors.New("muze 同一请求对应的订单不唯一或套餐不符")
		}
		id = o.ID
	}
	if id == "" {
		return nil, errors.New("muze 尚未返回此请求的订单")
	}
	c, err := p.Cycle(ctx, token, id)
	if err != nil {
		return nil, err
	}
	if c.RequestID != "" && c.RequestID != j.ID {
		return nil, errors.New("muze 订单详情与原请求不匹配")
	}
	c.RequestID = j.ID
	return c, nil
}
