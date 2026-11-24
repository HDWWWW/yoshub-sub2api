package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const accountRiskCustomStreamKey = "account_risk_custom_stream"
const riskErrorBodyLimit = 32 << 10

func (c AccountRiskConfig) validateCustomErrors() error {
	if c.CustomErrorWindowSeconds < 0 || c.CustomErrorWindowSeconds > 3600 || c.CustomErrorThreshold < 0 || c.CustomErrorThreshold > 10000 || (c.CustomErrorEnabled && (c.CustomErrorWindowSeconds == 0 || c.CustomErrorThreshold == 0)) {
		return errors.New("自定义错误时间窗口须为 1–3600 秒，次数须为 1–10000")
	}
	if len(c.CustomErrorRules) > 20 || (c.CustomErrorEnabled && len(c.CustomErrorRules) == 0) {
		return errors.New("请设置 1–20 条错误匹配规则")
	}
	for _, rule := range c.CustomErrorRules {
		text := strings.TrimSpace(rule.Keyword)
		if rule.Status != 0 && (rule.Status < 400 || rule.Status > 599) || rule.Status == 0 && text == "" || utf8.RuneCountInString(text) > 128 || strings.ContainsFunc(rule.Keyword, unicode.IsControl) {
			return errors.New("每条规则须填写 400–599 状态码或 1–128 字关键字，关键字不可含控制字符")
		}
	}
	return nil
}

func matchesCustomError(rules []AccountRiskErrorMatch, status int, body []byte) bool {
	var lower string
	for _, r := range rules {
		if r.Status != 0 && r.Status != status {
			continue
		}
		if r.Keyword == "" {
			return r.Status != 0
		}
		if len(body) == 0 {
			continue
		}
		if lower == "" {
			lower = strings.ToLower(string(body))
		}
		if strings.Contains(lower, strings.ToLower(r.Keyword)) {
			return true
		}
	}
	return false
}

// Cache only admission to error observation. A match re-reads the current rule
// and the stop transaction verifies its revision and the account identity.
type accountRiskCustomCache struct {
	mu     sync.Mutex
	until  time.Time
	groups map[int64][]AccountRiskErrorMatch
}

func (c *accountRiskCustomCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Time{}
}
func (s *AccountRiskService) customErrorRules(ctx context.Context, account *Account) []AccountRiskErrorMatch {
	if s == nil || s.store == nil || s.counter429 == nil || account == nil || account.Platform != PlatformOpenAI || len(account.GroupIDs) == 0 {
		return nil
	}
	c := &s.customErrorCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().After(c.until) {
		read, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		state, err := s.store.Read(read)
		cancel()
		c.until = time.Now().Add(time.Second)
		if err != nil {
			slog.Warn("custom error rule snapshot unavailable", "error", err)
		} else {
			groups := make(map[int64][]AccountRiskErrorMatch)
			for id, r := range state.Rules {
				if r != nil && r.Config.Enabled && r.Config.CustomErrorEnabled {
					groups[id] = slices.Clone(r.Config.CustomErrorRules)
				}
			}
			c.groups = groups
		}
	}
	if len(account.GroupIDs) == 1 {
		return c.groups[account.GroupIDs[0]]
	}
	var rules []AccountRiskErrorMatch
	for _, id := range account.GroupIDs {
		rules = append(rules, c.groups[id]...)
	}
	return rules
}

// Read error fields only. A response.failed envelope can also contain generated
// text, tool arguments, or partial output unrelated to the failure.
func riskErrorText(body []byte, stream bool) []byte {
	if len(body) == 0 {
		return nil
	}
	for _, path := range []string{"response.error", "error", "detail", "message"} {
		v := gjson.GetBytes(body, path)
		if v.Exists() && v.Type != gjson.Null {
			value := v.String()
			return []byte(value[:min(len(value), riskErrorBodyLimit)])
		}
	}
	if stream {
		return nil
	}
	return body[:min(len(body), riskErrorBodyLimit)]
}

func (s *OpenAIGatewayService) customRiskStream(ctx context.Context, a *Account) func([]byte) {
	if s == nil || s.accountRisk == nil || s.accountRisk.counter429 == nil || a == nil || a.Platform != PlatformOpenAI {
		return nil
	}
	// The wire event and the terminal representation can both describe one turn.
	// Generate the ID lazily so successful streams do not need a UUID.
	var once sync.Once
	return func(payload []byte) {
		observedAt := time.Now().UTC()
		rules := s.accountRisk.customErrorRules(ctx, a)
		if len(rules) == 0 {
			return
		}
		status, message := explicitStreamStatus(payload), riskErrorText(payload, true)
		if status == 0 && openAIStreamCredentialAuthFailure(payload) {
			status = http.StatusUnauthorized
		}
		if !matchesCustomError(rules, status, message) {
			return
		}
		once.Do(func() { s.observeRiskBurst(ctx, a, status, message, true, uuid.NewString(), observedAt) })
	}
}

func (s *OpenAIGatewayService) observeRiskHandshake(ctx context.Context, a *Account, status int, body []byte) {
	observedAt := time.Now().UTC()
	s.observeBusiness401(ctx, a, status, "WebSocket 握手")
	if s == nil || status < 400 {
		return
	}
	rules := s.accountRisk.customErrorRules(ctx, a)
	if len(rules) == 0 {
		return
	}
	msg := riskErrorText(body, false)
	if matchesCustomError(rules, status, msg) {
		s.observeRiskBurst(ctx, a, status, msg, true, uuid.NewString(), observedAt)
	}
}

func (s *OpenAIGatewayService) observeRiskHTTP(ctx context.Context, a *Account, resp *http.Response) {
	if s == nil || resp.StatusCode < 400 || resp.StatusCode > 599 {
		return
	}
	status := resp.StatusCode
	observedAt := time.Now().UTC()
	s.observeBusiness401(ctx, a, status, "HTTP")
	rules := s.accountRisk.customErrorRules(ctx, a)
	if len(rules) == 0 {
		return
	}
	observe := func(body []byte) {
		msg := riskErrorText(body, false)
		if matchesCustomError(rules, status, msg) {
			s.observeRiskBurst(ctx, a, status, msg, true, uuid.NewString(), observedAt)
		}
	}
	if resp.Body == nil {
		observe(nil)
		return
	}
	// The ordinary consumer owns the read deadline and Close. Capture at most an
	// error prefix, without a second upstream read or changes to transport errors.
	resp.Body = &riskErrorBody{ReadCloser: resp.Body, observe: observe}
}

type riskErrorBody struct {
	io.ReadCloser
	mu      sync.Mutex
	prefix  bytes.Buffer
	observe func([]byte)
}

func (b *riskErrorBody) finish(chunk []byte, done bool) {
	b.mu.Lock()
	observe := b.observe
	if observe == nil {
		b.mu.Unlock()
		return
	}
	n := min(len(chunk), riskErrorBodyLimit-b.prefix.Len())
	b.prefix.Write(chunk[:n])
	if !done && b.prefix.Len() < riskErrorBodyLimit {
		b.mu.Unlock()
		return
	}
	b.observe = nil
	prefix := b.prefix.Bytes()
	b.prefix = bytes.Buffer{}
	b.mu.Unlock()
	observe(prefix)
}
func (b *riskErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.finish(p[:n], err != nil)
	return n, err
}
func (b *riskErrorBody) Close() error { err := b.ReadCloser.Close(); b.finish(nil, true); return err }
