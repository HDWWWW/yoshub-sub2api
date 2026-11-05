package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"golang.org/x/time/rate"
)

var errSmartWSCapacity = errors.New("upstream WebSocket capacity exhausted before dispatch")

func (s *OpenAIGatewayService) openAIWSRequestLimit() int64 {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.MaxRequestBytes > 0 {
		return s.cfg.Gateway.OpenAIWS.MaxRequestBytes
	}
	return openAIWSMessageReadLimitBytes
}

func (p *openAIWSConnPool) upstreamFrameLimit() int64 {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.MaxUpstreamFrameBytes > 0 {
		return p.cfg.Gateway.OpenAIWS.MaxUpstreamFrameBytes
	}
	return openAIWSMessageReadLimitBytes
}

// Digests stay local: neither credentials nor header values enter metrics.
func smartWSHandshakeDigest(req openAIWSAcquireRequest) [32]byte {
	headers := make(map[string][]string)
	for name, values := range req.Headers {
		key := strings.ToLower(name)
		switch key {
		case "content-length", "accept-encoding", "x-request-id", "x-client-request-id", "traceparent", "tracestate", "x-codex-turn-metadata", "x-codex-routing-hint":
			continue
		}
		headers[key] = append(headers[key], values...)
	}
	for key := range headers {
		sort.Strings(headers[key])
	}
	// These capability headers are sets; equivalent ordering/whitespace must
	// not defeat safe reuse.
	for _, key := range []string{"openai-beta", "x-codex-beta-features"} {
		if values, ok := headers[key]; ok {
			set := map[string]bool{}
			for _, value := range values {
				for _, part := range strings.Split(value, ",") {
					if part = strings.TrimSpace(part); part != "" {
						set[part] = true
					}
				}
			}
			values = nil
			for value := range set {
				values = append(values, value)
			}
			sort.Strings(values)
			headers[key] = values
		}
	}
	accountType, taskID := "", ""
	accountID := int64(0)
	if req.Account != nil {
		accountType, taskID = req.Account.Type, req.Account.GetCredential("task_id")
		accountID = req.Account.ID
	}
	data, _ := json.Marshal([]any{strings.TrimSpace(req.WSURL), strings.TrimSpace(req.ProxyURL), accountID, accountType, taskID, headers})
	return sha256.Sum256(data)
}

type smartWSIdentityCapacity struct {
	conns, queued, dialing int
	limiter                *rate.Limiter
}

// One gate per process bounds all pool dials, including background prewarms.
// Existing account admission and native ingress limits remain additional caps.
type smartWSPoolGate struct {
	mu                     sync.Mutex
	conns, queued, dialing int
	identities             map[[32]byte]*smartWSIdentityCapacity
	limiter                *rate.Limiter
	accounts               map[int64]int
}

func (p *openAIWSConnPool) smartWSLimits() config.GatewayOpenAIWSConfig {
	var c config.GatewayOpenAIWSConfig
	if p != nil && p.cfg != nil {
		c = p.cfg.Gateway.OpenAIWS
	}
	// Zero-valued structs used by older callers retain bounded defaults.
	if c.MaxConnsGlobal <= 0 {
		c.MaxConnsGlobal = 512
	}
	if c.MaxConnsPerIdentity <= 0 {
		c.MaxConnsPerIdentity = 32
	}
	if c.MaxQueuedGlobal <= 0 {
		c.MaxQueuedGlobal = 128
	}
	if c.MaxQueuedPerIdentity <= 0 {
		c.MaxQueuedPerIdentity = 16
	}
	if c.MaxConcurrentHandshakes <= 0 {
		c.MaxConcurrentHandshakes = 8
	}
	if c.MaxConcurrentHandshakesPerIdentity <= 0 {
		c.MaxConcurrentHandshakesPerIdentity = 2
	}
	if c.HandshakesPerSecond <= 0 {
		c.HandshakesPerSecond = 8
	}
	if c.HandshakesPerSecondPerIdentity <= 0 {
		c.HandshakesPerSecondPerIdentity = 2
	}
	return c
}

func (g *smartWSPoolGate) identityLocked(key [32]byte, c config.GatewayOpenAIWSConfig) *smartWSIdentityCapacity {
	if g.identities == nil {
		g.identities = make(map[[32]byte]*smartWSIdentityCapacity)
	}
	if g.limiter == nil {
		g.limiter = rate.NewLimiter(rate.Limit(c.HandshakesPerSecond), c.MaxConcurrentHandshakes)
	}
	i := g.identities[key]
	if i == nil {
		i = &smartWSIdentityCapacity{limiter: rate.NewLimiter(rate.Limit(c.HandshakesPerSecondPerIdentity), c.MaxConcurrentHandshakesPerIdentity)}
		g.identities[key] = i
	}
	return i
}

func (g *smartWSPoolGate) pruneLocked(key [32]byte) {
	i := g.identities[key]
	if i != nil && i.conns == 0 && i.queued == 0 && i.dialing == 0 {
		// Retain a bounded idle rate-limit state. Eviction cannot bypass the
		// process-wide handshake limiter; active entries are never evicted.
		if len(g.identities) > 8192 {
			delete(g.identities, key)
		}
	}
}

func (p *openAIWSConnPool) enterSmartWSAcquire(req openAIWSAcquireRequest) (func(), error) {
	g, c, key := &p.gate, p.smartWSLimits(), smartWSCapacityIdentity(req)
	g.mu.Lock()
	if g.queued >= c.MaxQueuedGlobal {
		g.mu.Unlock()
		p.smartMetrics.queueRejected.Add(1)
		return nil, errSmartWSCapacity
	}
	i := g.identityLocked(key, c)
	if i.queued >= c.MaxQueuedPerIdentity {
		g.mu.Unlock()
		p.smartMetrics.queueRejected.Add(1)
		return nil, errSmartWSCapacity
	}
	g.queued++
	i.queued++
	g.mu.Unlock()
	return func() { g.mu.Lock(); g.queued--; i.queued--; g.pruneLocked(key); g.mu.Unlock() }, nil
}

func (p *openAIWSConnPool) reserveSmartWSDial(ctx context.Context, req openAIWSAcquireRequest) (releaseConn func(), finishDial func(), err error) {
	g, c, key := &p.gate, p.smartWSLimits(), smartWSCapacityIdentity(req)
	accountID := int64(0)
	if req.Account != nil {
		accountID = req.Account.ID
	}
	g.mu.Lock()
	if g.conns >= c.MaxConnsGlobal || g.dialing >= c.MaxConcurrentHandshakes || g.accounts[accountID] >= p.effectiveMaxConnsByAccount(req.Account) {
		g.mu.Unlock()
		p.smartMetrics.capacityRejected.Add(1)
		return nil, nil, errSmartWSCapacity
	}
	i := g.identityLocked(key, c)
	if g.conns >= c.MaxConnsGlobal || i.conns >= c.MaxConnsPerIdentity || g.dialing >= c.MaxConcurrentHandshakes || i.dialing >= c.MaxConcurrentHandshakesPerIdentity {
		g.mu.Unlock()
		p.smartMetrics.capacityRejected.Add(1)
		return nil, nil, errSmartWSCapacity
	}
	g.conns++
	if g.accounts == nil {
		g.accounts = make(map[int64]int)
	}
	g.accounts[accountID]++
	i.conns++
	g.dialing++
	i.dialing++
	globalLimiter, identityLimiter := g.limiter, i.limiter
	g.mu.Unlock()
	var connOnce, dialOnce sync.Once
	releaseConn = func() {
		connOnce.Do(func() {
			g.mu.Lock()
			g.conns--
			i.conns--
			g.accounts[accountID]--
			if g.accounts[accountID] == 0 {
				delete(g.accounts, accountID)
			}
			g.pruneLocked(key)
			g.mu.Unlock()
		})
	}
	finishDial = func() {
		dialOnce.Do(func() { g.mu.Lock(); g.dialing--; i.dialing--; g.pruneLocked(key); g.mu.Unlock() })
	}
	if err = globalLimiter.Wait(ctx); err == nil {
		err = identityLimiter.Wait(ctx)
	}
	if err != nil {
		finishDial()
		releaseConn()
		return nil, nil, err
	}
	return releaseConn, finishDial, nil
}

func (p *openAIWSConnPool) configuredIdleTTL() time.Duration {
	if p.cfg != nil && p.cfg.Gateway.OpenAIWS.IdleTimeoutSeconds > 0 {
		return time.Duration(p.cfg.Gateway.OpenAIWS.IdleTimeoutSeconds) * time.Second
	}
	return 5 * time.Minute
}

func mergeSmartWSBetaHeader(headers http.Header) {
	value := headers.Get("OpenAI-Beta")
	if !strings.Contains(value, "responses_websockets=") {
		if strings.TrimSpace(value) != "" {
			value += ", "
		}
		headers.Set("OpenAI-Beta", value+openAIWSBetaV2Value)
	}
}

func smartWSCapacityIdentity(req openAIWSAcquireRequest) [32]byte {
	h := make(http.Header)
	for _, name := range []string{"Authorization", "OpenAI-Project", "OpenAI-Organization", "Chatgpt-Account-Id"} {
		if value := req.Headers.Get(name); value != "" {
			h.Set(name, value)
		}
	}
	req.Headers = h
	// The same upstream credential entered as multiple accounts still shares
	// one identity cap. Account-level capacity is enforced separately.
	req.Account = nil
	// Changing a proxy cannot obtain an extra allowance for the same upstream
	// authentication identity. Proxy isolation still applies to actual reuse.
	req.ProxyURL = ""
	return smartWSHandshakeDigest(req)
}

func (m *smartWSMetrics) recordHandshakeFailure(status int) {
	m.handshakeFailed.Add(1)
	switch status {
	case 401, 403:
		m.handshakeAuth.Add(1)
	case 429:
		m.handshakeRateLimited.Add(1)
	case 404, 405, 426, 501:
		m.handshakeUnsupported.Add(1)
	}
}
