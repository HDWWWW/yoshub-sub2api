package service

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type smartWSHandshakeKey struct{}

// Provenance is internal response context, never a client-controlled header.
type smartWSHandshakeFailure struct {
	category       string
	accountFailure bool
}

func (f smartWSHandshakeFailure) annotate(err *UpstreamFailoverError) {
	err.Stage = GatewayFailureStageWSHandshake
	err.Scope = GatewayFailureScopeProvider
	err.Reason = GatewayFailureReason(f.category)
	if f.accountFailure {
		err.Scope = GatewayFailureScopeAccount
	}
}

func classifySmartWSHandshake(status int, body []byte) smartWSHandshakeFailure {
	switch status {
	case http.StatusUnauthorized:
		return smartWSHandshakeFailure{"http_401", true}
	case http.StatusForbidden:
		if openAIStream403AccountFailure(body, "") {
			return smartWSHandshakeFailure{"credential_rejected", true}
		}
		return smartWSHandshakeFailure{"ws_forbidden", false}
	case http.StatusTooManyRequests:
		return smartWSHandshakeFailure{"rate_limited", true}
	case 529:
		return smartWSHandshakeFailure{"upstream_overloaded", true}
	case 404, 405, 426, 501:
		return smartWSHandshakeFailure{"ws_unsupported", false}
	default:
		return smartWSHandshakeFailure{"upstream_error", false}
	}
}

func smartWSHandshakeErrorResponse(req *http.Request, dialErr *openAIWSDialError) *http.Response {
	classification := classifySmartWSHandshake(dialErr.StatusCode, dialErr.ResponseBody)
	resp := openAIHTTPWSErrorResponse(req, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody)
	resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), smartWSHandshakeKey{}, classification))
	return resp
}

// Only route subsequent independent requests around an endpoint rejection.
// This cache never retries the rejected turn, changes account health, or caches
// 401/429/network failures. Native WS does not consult it. Entries are bounded
// and expire without extension from cache hits.
type smartWSRejectionCache struct {
	mu      sync.Mutex
	entries map[[32]byte]time.Time
}

func smartWSRejectionDigest(req openAIWSAcquireRequest) [32]byte {
	req.Headers = req.Headers.Clone()
	// Session affinity is required for connection reuse, but does not change
	// endpoint authorization. Retain all authentication and capability headers.
	for _, name := range []string{"session_id", "conversation_id", "thread-id", "x-codex-turn-state"} {
		req.Headers.Del(name)
	}
	return smartWSHandshakeDigest(req)
}

func (c *smartWSRejectionCache) active(key [32]byte, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.entries[key]
	if ok && !now.Before(until) {
		delete(c.entries, key)
		return false
	}
	return ok
}

func (c *smartWSRejectionCache) put(key [32]byte, now time.Time, ttl time.Duration, capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl <= 0 || capacity <= 0 {
		return
	}
	if c.entries == nil {
		c.entries = make(map[[32]byte]time.Time)
	}
	if until, ok := c.entries[key]; ok && now.Before(until) {
		return
	}
	if len(c.entries) >= capacity {
		var oldest [32]byte
		var earliest time.Time
		for k, until := range c.entries {
			if !now.Before(until) {
				delete(c.entries, k)
				continue
			}
			if earliest.IsZero() || until.Before(earliest) {
				oldest, earliest = k, until
			}
		}
		if len(c.entries) >= capacity {
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = now.Add(ttl)
}

func (p *openAIWSConnPool) rememberSmartWSRejection(req openAIWSAcquireRequest, dialErr *openAIWSDialError) {
	category := classifySmartWSHandshake(dialErr.StatusCode, dialErr.ResponseBody).category
	if category != "ws_forbidden" && category != "ws_unsupported" {
		return
	}
	cfg := p.smartWSLimits()
	key := smartWSRejectionDigest(req)
	// HeadersFactory may have refreshed the authentication at dial time.
	if dialErr.rejectionKey != ([32]byte{}) {
		key = dialErr.rejectionKey
	}
	p.rejections.put(key, time.Now(), time.Duration(cfg.HandshakeRejectionCooldownSeconds)*time.Second, cfg.MaxHandshakeRejections)
	p.smartMetrics.handshakeRejection.Add(1)
}
