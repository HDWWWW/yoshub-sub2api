package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	accountRiskBusiness401 = "business_401"
	accountRiskAccount401  = "account_401"
	risk401HistoryOnly     = "history_only"
)

// Standalone from the scheduler so account maintenance clients can report actual
// responses even when neither scheduling nor the automatic probe is enabled.
type Account401Reporter struct {
	store AccountRiskStore
	wake  chan struct{}
}

func NewAccount401Reporter(store AccountRiskStore) *Account401Reporter {
	return &Account401Reporter{store: store, wake: make(chan struct{}, 1)}
}

func (s *AccountRiskService) reporter401() *Account401Reporter {
	if s == nil {
		return nil
	}
	return &Account401Reporter{store: s.store, wake: s.notificationWake}
}

func (s *OpenAIGatewayService) reporter401() *Account401Reporter {
	if s == nil {
		return nil
	}
	return s.accountRisk.reporter401()
}

// Usage reads may collect several HTTP responses (including credential setup).
// Keep their audit events without reserving a group's next 401 notification.
type usage401AuditKey struct{}

func withUsage401Audit(ctx context.Context) context.Context {
	return context.WithValue(ctx, usage401AuditKey{}, true)
}

type risk401GroupKey struct{}

func risk401RequestGroup(ctx context.Context) int64 {
	if id, _ := ctx.Value(risk401GroupKey{}).(int64); id > 0 {
		return id
	}
	if group, _ := ctx.Value(ctxkey.Group).(*Group); group != nil && group.ID > 0 {
		return group.ID
	}
	if scope := headerOverrideScopeFromContext(ctx); scope != nil {
		return scope.groupID
	}
	return 0
}

func (s *OpenAIGatewayService) observeBusiness401(ctx context.Context, account *Account, status int, source string) {
	if status != http.StatusUnauthorized {
		return
	}
	groupID := risk401RequestGroup(ctx)
	if groupID > 0 {
		s.recordBusiness401(ctx, groupID, account, source)
		return
	}
	s.reporter401().observe(ctx, 0, account, status, source)
}

func (s *OpenAIGatewayService) recordBusiness401(ctx context.Context, groupID int64, account *Account, source string) {
	s.reporter401().record(ctx, groupID, account, accountRiskBusiness401, "gateway", "业务请求收到上游 401（"+source+"）")
}

func (r *Account401Reporter) observe(ctx context.Context, groupID int64, account *Account, status int, source string) {
	if r != nil && status == http.StatusUnauthorized {
		r.record(ctx, groupID, account, accountRiskAccount401, "observed", source+"收到上游 401")
	}
}

func (r *Account401Reporter) observeID(ctx context.Context, accountID int64, status int, source string) {
	if status == http.StatusUnauthorized {
		at := time.Now().UTC()
		r.enqueue(ctx, risk401RequestGroup(ctx), r.targetAt401(ctx, accountID, accountID), at, accountRiskAccount401, "observed", source+"收到上游 401")
	}
}

// Only an actual grant/validated PAT or a completed authenticated probe can
// rearm notifications. The check must have started after the last observed 401.
func (r *Account401Reporter) recovered(ctx context.Context, owner *Account, startedAt time.Time) {
	if r == nil || r.store == nil || owner == nil || owner.ID <= 0 || owner.Platform != PlatformOpenAI {
		return
	}
	id := owner.ID
	if owner.IsShadow() {
		id = *owner.ParentAccountID
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := r.store.Recover401(persist, id, startedAt); err != nil {
		slog.Warn("account_risk_401_recovery_failed", "account_id", id, "error", err)
	}
}

type account401ScopeKey struct{}
type account401Scope struct {
	reporter  *Account401Reporter
	accountID int64
	source    string
}

func (r *Account401Reporter) withScope(ctx context.Context, id int64, source string) context.Context {
	if r == nil || id <= 0 {
		return ctx
	}
	return context.WithValue(ctx, account401ScopeKey{}, account401Scope{reporter: r, accountID: id, source: source})
}

// ObserveAccountHTTPStatus is called once by a scoped maintenance transport for
// each physical response, before mappings, fallbacks or credential refresh retries.
func ObserveAccountHTTPStatus(ctx context.Context, status int) {
	if status != http.StatusUnauthorized {
		return
	}
	if scope, ok := ctx.Value(account401ScopeKey{}).(account401Scope); ok {
		scope.reporter.observeID(ctx, scope.accountID, status, scope.source)
	}
}

// A scoped request belongs only to its request group. Admin and background
// requests belong to each explicitly subscribed group of the affected account.
func (r *Account401Reporter) record(ctx context.Context, groupID int64, account *Account, kind, action, reason string) {
	if account == nil || account.Platform != PlatformOpenAI {
		return
	}
	at := time.Now().UTC()
	ownerID := account.ID
	if account.IsShadow() {
		ownerID = *account.ParentAccountID
	}
	r.enqueue(ctx, groupID, r.targetAt401(ctx, account.ID, ownerID), at, kind, action, reason)
}

// Use the database state when the response arrives, before authentication handlers
// can stop the account. Never re-evaluate this decision during a queue retry.
func (r *Account401Reporter) targetAt401(ctx context.Context, sourceID, ownerID int64) *Account401AlertTarget {
	if r == nil || r.store == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	target, err := r.store.Lookup401AlertTarget(readCtx, sourceID, ownerID)
	if err != nil {
		slog.Warn("account_risk_401_target_unavailable", "account_id", sourceID, "error", err)
		return &Account401AlertTarget{}
	}
	return &target
}

func (r *Account401Reporter) enqueue(ctx context.Context, groupID int64, target *Account401AlertTarget, at time.Time, kind, action, reason string) {
	if target == nil || target.AccountID <= 0 {
		return
	}
	eligible := target.eligibleAt(at)
	auditOnly, _ := ctx.Value(usage401AuditKey{}).(bool)
	if !eligible {
		reason += risk401ReclaimNote
	}
	ids := target.GroupIDs
	if groupID > 0 {
		ids = []int64{groupID}
	}
	if len(ids) == 0 {
		return
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for {
		queued := false
		err := r.store.Update(persist, func(state *AccountRiskState, tx AccountRiskScheduling) error {
			for id, rule := range state.Rules {
				if rule == nil || !rule.Config.Notify401Enabled || !slices.Contains(ids, id) {
					continue
				}
				event := AccountRiskEvent{At: at, AccountID: target.AccountID, AccountName: target.Name,
					AccountEmail: riskNotificationEmail(target.Email, target.Name), Kind: kind, Action: action, Reason: reason}
				if auditOnly {
					event.Notification = risk401HistoryOnly
				}
				if err := appendRisk401Event(rule, tx, target.OwnerID, event, eligible && !auditOnly); err != nil {
					return err
				}
				queued = queued || rule.Events[0].Notification == "pending"
			}
			return nil
		})
		if err == nil {
			if queued {
				select {
				case r.wake <- struct{}{}:
				default:
				}
			}
			return
		}
		if !errors.Is(err, ErrAccountRiskBusy) || persist.Err() != nil {
			slog.Warn("account_risk_401_alert_failed", "group_id", groupID, "account_id", target.AccountID, "error", err)
			return
		}
		select {
		case <-persist.Done():
			slog.Warn("account_risk_401_alert_timeout", "group_id", groupID, "account_id", target.AccountID)
			return
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (s *AccountTestService) observeTest401(ctx context.Context, account *Account, status int, source string) {
	if status == http.StatusUnauthorized && riskProbeForContext(ctx) == nil {
		s.openaiGatewayService.reporter401().observe(ctx, risk401RequestGroup(ctx), account, status, source)
	}
}

func (s *AccountTestService) observeTest401Frame(c *gin.Context, account *Account, raw string) {
	if explicitStream401([]byte(raw)) {
		s.observeTest401(c.Request.Context(), account, http.StatusUnauthorized, "手动测试流式响应")
	}
}

// Some transports deliver a typed error after a successful HTTP upgrade. Two
// frames (error + response.failed) may describe the same rejected turn. The
// observer is per attempt/turn, and accepts only an explicit upstream status.
func (s *OpenAIGatewayService) accountRiskStream401(ctx context.Context, c *gin.Context, account *Account) func() {
	if s == nil || s.accountRisk == nil || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	groupID := getOpenAIGroupIDFromContext(c)
	return sync.OnceFunc(func() {
		if groupID > 0 {
			s.recordBusiness401(ctx, groupID, account, "流式错误事件")
			return
		}
		s.observeBusiness401(ctx, account, http.StatusUnauthorized, "流式错误事件")
	})
}

// Embedded error envelopes can arrive over an otherwise successful HTTP request.
// Inspect only structured upstream errors. Ordinary output text is never evidence.
func embeddedAccount401(payload []byte, eventType string) bool {
	if eventType != "error" && eventType != "response.failed" && !gjson.GetBytes(payload, "error").IsObject() && !gjson.GetBytes(payload, "response.error").IsObject() {
		return false
	}
	return explicitStream401(payload)
}

func bodyHasAccount401(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || (!bytes.Contains(body, []byte(`"error"`)) && !bytes.Contains(body, []byte("response.failed")) && !bytes.Contains(body, []byte("event: error"))) {
		return false
	}
	if body[0] == '{' {
		return embeddedAccount401(body, gjson.GetBytes(body, "type").Str)
	}
	found := false
	forEachOpenAISSEFrame(string(body), func(eventType string, payload []byte) {
		if !found && embeddedAccount401(payload, eventType) {
			found = true
		}
	})
	return found
}

func (s *AccountTestService) observeTest401Body(ctx context.Context, account *Account, status int, body []byte, source string) {
	if status >= 200 && status < 300 && account != nil && account.Platform == PlatformOpenAI && s.openaiGatewayService.reporter401() != nil && bodyHasAccount401(body) {
		s.observeTest401(ctx, account, http.StatusUnauthorized, source)
	}
}

func (o *upstreamResponseModelObserver) observe401Body(body []byte) {
	if o != nil && o.on401 != nil && bodyHasAccount401(body) {
		o.on401()
	}
}

func (o *upstreamResponseModelObserver) observe401Frame(payload []byte, eventType string) {
	if o != nil && o.on401 != nil && embeddedAccount401(payload, eventType) {
		o.on401()
	}
}

func explicitStreamStatus(payload []byte) int {
	for _, path := range openAIStreamErrorStatusPaths {
		v := gjson.GetBytes(payload, path)
		if v.Type != gjson.Number && v.Type != gjson.String {
			continue
		}
		if n, err := strconv.Atoi(v.String()); err == nil && n >= 400 && n <= 599 {
			return n
		}
	}
	return 0
}

func explicitStream401(payload []byte) bool {
	if status := explicitStreamStatus(payload); status != 0 {
		return status == http.StatusUnauthorized
	}
	// A typed authentication rejection is the stream equivalent of HTTP 401.
	return openAIStreamCredentialAuthFailure(payload)
}
