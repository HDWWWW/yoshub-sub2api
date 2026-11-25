package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const refillProvider558669 = "558669"
const refill558669URL = "https://gptpro.558669.xyz/api/customer/developer/v1"

type http558669Provider struct{ *httpRefillProvider }

func newHTTP558669Provider() *http558669Provider {
	p := newHTTPRefillProvider()
	p.base, p.identityBase = refill558669URL, ""
	return &http558669Provider{p}
}

// The documented API has no /me or /keys endpoints. All money is in points,
// with available already excluding reserved supplier credit.
func (p *http558669Provider) Identity(ctx context.Context, token string) (*RefillWallet, *RefillKeyInfo, error) {
	c, err := p.Catalog(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	return &RefillWallet{Balance: c.Available + c.Reserved, Held: c.Reserved, Available: c.Available}, nil, nil
}

func (p *http558669Provider) Catalog(ctx context.Context, token string) (*RefillCatalog, error) {
	c, err := p.httpRefillProvider.Catalog(ctx, token)
	if err != nil {
		return nil, err
	}
	if c.Reserved < 0 || math.IsNaN(c.Reserved) || math.IsInf(c.Reserved, 0) {
		return nil, errors.New("558669 积分预占无效")
	}
	seen := map[string]bool{}
	for _, q := range c.Items {
		if q.Service != "refill" {
			continue
		}
		if seen[q.Plan] {
			return nil, errors.New("558669 返回重复套餐报价")
		}
		seen[q.Plan] = true
	}
	return c, nil
}

// Accept Unix seconds or RFC3339 timestamps. Null is unknown, never a fresh lease.
func decode558669Job(data json.RawMessage) (*RefillProviderJob, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("558669 任务响应无效")
	}
	var expires *int64
	if raw := fields["expires_at"]; len(raw) > 0 && string(raw) != "null" {
		var seconds int64
		if err := json.Unmarshal(raw, &seconds); err != nil {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return nil, errors.New("558669 租期格式无效")
			}
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, errors.New("558669 租期格式无效")
			}
			seconds = at.Unix()
		}
		expires = &seconds
	}
	delete(fields, "expires_at")
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var job RefillProviderJob
	if json.Unmarshal(body, &job) != nil {
		return nil, errors.New("558669 任务响应无效")
	}
	job.ExpiresAt = expires
	return &job, nil
}

func (p *http558669Provider) Submit(ctx context.Context, token string, j AccountRefillJob) (*RefillProviderJob, error) {
	var raw json.RawMessage
	err := p.request(ctx, token, http.MethodPost, "/refills", "sub2-refill-"+j.ID,
		map[string]any{"plan": j.Plan, "format": "sub2api", "price_version": j.PriceVersion}, &raw)
	if err != nil {
		return nil, err
	}
	return decode558669Job(raw)
}

func (p *http558669Provider) Status(ctx context.Context, token, id string) (*RefillProviderJob, error) {
	if !valid558669ID(id) {
		return nil, errors.New("558669 原任务编号无效")
	}
	var raw json.RawMessage
	if err := p.request(ctx, token, http.MethodGet, "/refills/"+url.PathEscape(id), "", nil, &raw); err != nil {
		return nil, err
	}
	return decode558669Job(raw)
}

func valid558669ID(id string) bool {
	return validMuzeIdentifier(id, false)
}

func validate558669Job(remote *RefillProviderJob, job AccountRefillJob) error {
	if err := validateRefillProviderJob(remote, job); err != nil {
		return err
	}
	if !valid558669ID(remote.ID) || remote.DeliveryVersion < 0 || remote.PriceVersion != job.PriceVersion ||
		remote.Service != "refill" || remote.Plan != job.Plan || remote.Format != "sub2api" || remote.Quantity == nil || *remote.Quantity != 1 ||
		math.Abs(remote.Price-job.Price) > 1e-8 {
		return errors.New("558669 任务与原订单不一致，继续核对原任务")
	}
	return nil
}

func valid558669Token(token string) bool {
	if len(token) < 8 || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

func (s *AccountRefillService) providerClient(provider string) RefillProvider {
	if provider == refillProvider558669 {
		if s.provider558669 != nil {
			return s.provider558669
		}
		return newHTTP558669Provider()
	}
	return s.provider
}

func refill558669Version(extra map[string]any) int64 {
	switch v := extra["refill_delivery_version"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := strconv.ParseInt(string(v), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}
