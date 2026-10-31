package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const refillProviderURL = "https://pro.llmrefund.com/api/customer/developer/v1"

type RefillQuote struct {
	Service      string  `json:"service"`
	Plan         string  `json:"plan"`
	Price        float64 `json:"price"`
	PriceVersion int64   `json:"price_version"`
	Enabled      bool    `json:"enabled"`
	Terms        string  `json:"terms"`
}
type RefillCatalog struct {
	Available float64       `json:"available"`
	Items     []RefillQuote `json:"items"`
}
type RefillWallet struct {
	CustomerID string  `json:"id"`
	Balance    float64 `json:"balance"`
	Held       float64 `json:"held"`
	Available  float64 `json:"available"`
}
type RefillKeyInfo struct {
	Label     string    `json:"label"`
	Prefix    string    `json:"prefix"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}
type RefillProviderJob struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Price       float64 `json:"price"`
	ExpiresAt   *int64  `json:"expires_at"`
	ResultReady bool    `json:"result_ready"`
}
type RefillDelivery struct {
	Accounts []struct {
		Name        string         `json:"name"`
		Platform    string         `json:"platform"`
		Type        string         `json:"type"`
		Credentials map[string]any `json:"credentials"`
		ExpiresAt   *int64         `json:"expires_at"`
	} `json:"accounts"`
}
type RefillProvider interface {
	Catalog(context.Context, string) (*RefillCatalog, error)
	Submit(context.Context, string, AccountRefillJob) (*RefillProviderJob, error)
	Status(context.Context, string, string) (*RefillProviderJob, error)
	Delivery(context.Context, string, string) (*RefillDelivery, error)
	Identity(context.Context, string) (*RefillWallet, *RefillKeyInfo, error)
}

type refillProviderError struct {
	code       int
	retryAfter time.Duration
}

func (e *refillProviderError) Error() string {
	message := map[int]string{401: "令牌无效、已过期或被撤销", 402: "号商可用积分不足", 403: "权限不足，请确认读写和 JSON 导出权限", 404: "号商任务不存在", 409: "状态或报价冲突，请核对原任务", 410: "交付租期已结束", 422: "号商拒绝参数，请核对套餐与报价", 429: "号商请求限流"}[e.code]
	if message == "" {
		message = "号商连接暂时异常，保留原任务重试"
	}
	return fmt.Sprintf("%s（HTTP %d）", message, e.code)
}

type httpRefillProvider struct {
	client       *http.Client
	base         string
	identityBase string
}

func newHTTPRefillProvider() *httpRefillProvider {
	return &httpRefillProvider{base: refillProviderURL, identityBase: "https://pro.llmrefund.com/api/customer/refund-studio/v1", client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *httpRefillProvider) request(ctx context.Context, token, method, path, key string, body, out any) error {
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
		return errors.New("补号请求构建失败")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Sub2API/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New("号商连接中断，保留原任务重试")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := 15 * time.Second
		if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
			delay = time.Duration(refillMin(seconds, 86400)) * time.Second
		} else if at, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			delay = refillMax(delay, time.Until(at))
		}
		return &refillProviderError{resp.StatusCode, delay}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 || json.Unmarshal(data, out) != nil {
		return errors.New("号商响应不完整，保留原任务重试")
	}
	return nil
}
func (p *httpRefillProvider) Catalog(ctx context.Context, t string) (*RefillCatalog, error) {
	var out RefillCatalog
	if err := p.request(ctx, t, "GET", "/catalog", "", nil, &out); err != nil {
		return nil, err
	}
	if math.IsNaN(out.Available) || math.IsInf(out.Available, 0) || out.Available < 0 || len(out.Items) == 0 {
		return nil, errors.New("号商报价无效")
	}
	for _, q := range out.Items {
		if q.Price < 0 || math.IsNaN(q.Price) || math.IsInf(q.Price, 0) || q.PriceVersion <= 0 {
			return nil, errors.New("号商报价无效")
		}
	}
	return &out, nil
}
func (p *httpRefillProvider) Submit(ctx context.Context, t string, j AccountRefillJob) (*RefillProviderJob, error) {
	var out RefillProviderJob
	err := p.request(ctx, t, "POST", "/refills", "sub2-refill-"+j.ID, map[string]any{"plan": j.Plan, "format": "sub2api", "price_version": j.PriceVersion}, &out)
	return &out, err
}
func (p *httpRefillProvider) Status(ctx context.Context, t, id string) (*RefillProviderJob, error) {
	var out RefillProviderJob
	err := p.request(ctx, t, "GET", "/refills/"+url.PathEscape(id), "", nil, &out)
	return &out, err
}
func (p *httpRefillProvider) Delivery(ctx context.Context, t, id string) (*RefillDelivery, error) {
	var out RefillDelivery
	err := p.request(ctx, t, "GET", "/refills/"+url.PathEscape(id)+"/delivery", "", nil, &out)
	return &out, err
}
func (p *httpRefillProvider) Identity(ctx context.Context, token string) (*RefillWallet, *RefillKeyInfo, error) {
	identity := *p
	identity.base = p.identityBase
	var wallet RefillWallet
	if err := identity.request(ctx, token, "GET", "/me", "", nil, &wallet); err != nil {
		return nil, nil, err
	}
	// Legacy wallet API uses hundredths of a point; catalog prices use points.
	wallet.Balance /= 100
	wallet.Held /= 100
	wallet.Available /= 100
	for page := 1; page <= 5; page++ {
		var result struct {
			Items []RefillKeyInfo `json:"items"`
			Total int             `json:"total"`
		}
		if err := identity.request(ctx, token, "GET", fmt.Sprintf("/keys?page=%d&page_size=100", page), "", nil, &result); err != nil {
			return &wallet, nil, err
		}
		for _, key := range result.Items {
			if key.Prefix != "" && strings.HasPrefix(token, key.Prefix) && !key.Revoked {
				return &wallet, &key, nil
			}
		}
		if page*100 >= result.Total {
			break
		}
	}
	return &wallet, nil, nil
}
