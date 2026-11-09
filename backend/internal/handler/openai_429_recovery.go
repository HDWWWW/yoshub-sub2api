package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	openAI429RecoveryKey        = "openai429Recovery"
	openAI429RecoveryWindow     = 30 * time.Second
	openAI429RecoveryMaxRetries = 3
)

// The budget belongs to one downstream request (one turn for WS). Account
// health resets and account switches cannot renew it. The deadline bounds
// scheduling and retry starts, never an already running successful generation.
type openAI429Recovery struct {
	mu       sync.Mutex
	deadline time.Time
	retries  int
	limit    int
	lastErr  *service.UpstreamFailoverError
}

func openAI429RecoveryFor(c *gin.Context) *openAI429Recovery {
	if value, ok := c.Get(openAI429RecoveryKey); ok {
		if budget, valid := value.(*openAI429Recovery); valid {
			return budget
		}
	}
	budget := &openAI429Recovery{}
	c.Set(openAI429RecoveryKey, budget)
	return budget
}

func (b *openAI429Recovery) allow(account *service.Account, err *service.UpstreamFailoverError, sameAccountCount, retryLimit int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		if account == nil || !account.IsOpenAIOAuthLike() || err == nil || err.StatusCode != http.StatusTooManyRequests {
			return true
		}
		b.deadline = now.Add(openAI429RecoveryWindow)
		b.limit = openAI429RecoveryMaxRetries
	}
	if err == nil {
		return false
	}
	b.lastErr = err
	if b.retries >= b.limit || !now.Before(b.deadline) {
		return false
	}
	// Freeze the earliest deadline; a fresh account-level error may shorten it
	// but must never extend the downstream request's recovery window.
	if !err.SameAccountRetryDeadline.IsZero() && err.SameAccountRetryDeadline.Before(b.deadline) {
		b.deadline = err.SameAccountRetryDeadline
	}
	if !now.Before(b.deadline) {
		return false
	}
	err.SameAccountRetryDeadline = b.deadline
	if sameAccountRetryAllowed(err, sameAccountCount, retryLimit) {
		if !now.Add(sameAccountRetryDelayFor(err, sameAccountCount+1)).Before(b.deadline) {
			return false
		}
	}
	b.retries++
	return true
}

func (b *openAI429Recovery) expired(now time.Time) *service.UpstreamFailoverError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deadline.IsZero() && !now.Before(b.deadline) {
		return b.lastErr
	}
	return nil
}

func (b *openAI429Recovery) selectionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	b.mu.Lock()
	deadline := b.deadline
	b.mu.Unlock()
	if deadline.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func (b *openAI429Recovery) waitTimeout(timeout time.Duration, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		return timeout
	}
	// A nonpositive timeout means "use default" to the concurrency helper.
	// Keep an expired budget positive so it cannot become a fresh long wait.
	remaining := max(time.Nanosecond, b.deadline.Sub(now))
	if timeout <= 0 || remaining < timeout {
		return remaining
	}
	return timeout
}

func (b *openAI429Recovery) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deadline = time.Time{}
	b.retries, b.limit, b.lastErr = 0, 0, nil
}

func (h *OpenAIGatewayHandler) endExpiredOpenAI429Recovery(c *gin.Context, streamStarted bool) bool {
	if err := openAI429RecoveryFor(c).expired(time.Now()); err != nil {
		if !failoverClientGone(c) {
			requestLogger(c, "handler.openai_gateway").Warn("openai.429_recovery_deadline_exhausted")
			if c.Request != nil && strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/messages") {
				h.handleAnthropicFailoverExhausted(c, err, streamStarted)
			} else {
				h.handleFailoverExhausted(c, err, streamStarted)
			}
		}
		return true
	}
	return false
}

func allowOpenAI429Recovery(c *gin.Context, account *service.Account, err *service.UpstreamFailoverError, sameAccountCount int) bool {
	if openAI429RecoveryFor(c).allow(account, err, sameAccountCount, effectiveSameAccountRetryLimit(err, account), time.Now()) {
		return true
	}
	requestLogger(c, "handler.openai_gateway").Warn("openai.429_recovery_exhausted",
		zap.Int64("account_id", account.ID), zap.Int("max_retries", openAI429RecoveryMaxRetries),
		zap.Duration("recovery_window", openAI429RecoveryWindow))
	return false
}
