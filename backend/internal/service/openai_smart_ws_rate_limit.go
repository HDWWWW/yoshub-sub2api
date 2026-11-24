package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Later native turns must stay on their existing socket and account. This
// budget is reset only by a new client turn, never by an internal retry.
type smartWSNativeRetryBudget struct {
	attempts int
	deadline time.Time
}

func (b *smartWSNativeRetryBudget) wait(ctx context.Context, account *Account, failure *UpstreamFailoverError, turn int) bool {
	if failure == nil || !failure.RetryableOnSameAccount || ctx.Err() != nil {
		return false
	}
	limit := account.GetPoolModeRetryCount()
	if failure.SameAccountRetryMax > 0 {
		limit = min(limit, failure.SameAccountRetryMax)
	}
	if b.attempts >= limit {
		return false
	}
	if b.deadline.IsZero() {
		b.deadline = time.Now().Add(30 * time.Second)
		if !failure.SameAccountRetryDeadline.IsZero() && failure.SameAccountRetryDeadline.Before(b.deadline) {
			b.deadline = failure.SameAccountRetryDeadline
		}
	}
	delay := failure.SameAccountRetryDelay
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}
	if !time.Now().Add(delay).Before(b.deadline) {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return false
	}
	b.attempts++
	logOpenAIWSModeInfo("ingress_ws_same_account_retry account_id=%d turn=%d retry_count=%d retry_limit=%d upstream_status=%d", account.ID, turn, b.attempts, limit, failure.StatusCode)
	return true
}

// A confirmed rejection is distinct from both an unsent request and an
// uncertain dispatch. Native WS recovery still requires this proof.
const smartWSRejected int32 = -1

func (d *smartWSDispatch) observe(c *gin.Context, payload []byte) {
	if d == nil || len(payload) == 0 {
		return
	}
	// Observe every frame, including those after response.created established
	// acceptance. Empty message/reasoning scaffolding is still pre-output;
	// tool starts, opaque content and any usage object must prevent replay.
	typ := gjson.GetBytes(payload, "type").String()
	if d.httpPreOutputRecovery && !d.replayUnsafe.Load() {
		if gjson.GetBytes(payload, "response.usage").IsObject() || gjson.GetBytes(payload, "usage").IsObject() ||
			gjson.GetBytes(payload, "response.output.#").Int() > 0 || gjson.GetBytes(payload, "output.#").Int() > 0 ||
			openAIStreamDataStartsClientOutput(string(payload), typ) {
			d.replayUnsafe.Store(true)
		} else if typ == "response.output_item.added" {
			item := parseRawJSONView(payload).Get("item")
			itemType := item.Get("type").String()
			// Opaque output commits work even if the client-output predicate
			// sees only an otherwise empty message or reasoning scaffold.
			if (itemType != "message" && itemType != "reasoning") || item.Get("encrypted_content").String() != "" ||
				item.Get(`content.#(type=="encrypted_content")`).Exists() {
				d.replayUnsafe.Store(true)
			}
		}
	}
	var usage OpenAIUsage
	parseOpenAIWSResponseUsageFromCompletedEvent(payload, &usage)
	markOpenAICyberPolicyEvent(c, payload, http.StatusOK, &usage)
	mergeSmartWSCyberPolicyUsage(c, &usage)
	// Sticky for the whole turn, including errors found while draining a tail.
	// An unknown error can never be made retryable by a later 429 either.
	if isOpenAIContentPolicyRejection(payload) {
		if d.policy.Load() == nil {
			copy := append([]byte(nil), payload...)
			d.policy.CompareAndSwap(nil, &copy)
		}
		d.accepted.Store(true)
	}
	if d.accepted.Load() {
		return
	}
	if (typ == "error" || typ == "response.failed") && !smartWSIsExplicitRateLimit(payload) && !smartWSIsExplicitTemporaryRejection(payload) {
		d.accepted.Store(true)
	}
	// Even response.created establishes acceptance. A later failure may carry
	// usage or follow tool execution, regardless of downstream visibility.
	if typ != "error" && typ != "response.failed" &&
		(typ == "response.created" || typ == "response.in_progress" || typ == "response.queued" || !openAIStreamEventIsMetadata(typ)) {
		d.accepted.Store(true)
	}
	if smartWSResponseID(payload) != "" || smartWSPayloadHasWork(payload) {
		d.accepted.Store(true)
	}
}

// The first downstream error must carry an observed policy refusal. Sending a
// preceding quota error could make another gateway retry before reading it.
func (d *smartWSDispatch) failurePayload(payload []byte) []byte {
	if d != nil {
		if policy := d.policy.Load(); policy != nil && !isOpenAIContentPolicyRejection(payload) {
			return openAIPolicyRejectionPayload(*policy)
		}
	}
	return openAIPolicyRejectionPayload(payload)
}

func smartWSPayloadHasWork(payload []byte) bool {
	fields := gjson.GetManyBytes(payload, "response.usage", "usage", "response.output.#", "output.#")
	// A reported usage object (including zero or an unknown schema) means we
	// cannot prove rejection before execution. Preserve it for accounting.
	return fields[0].Exists() || fields[1].Exists() || fields[2].Int() > 0 || fields[3].Int() > 0
}

func smartWSIsExplicitRateLimit(payload []byte) bool {
	if !gjson.ValidBytes(payload) || smartWSResponseID(payload) != "" || smartWSPayloadHasWork(payload) || isOpenAIContentPolicyRejection(payload) {
		return false
	}
	// Do not infer a safe rejection from arbitrary error-message keywords.
	found := false
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "":
		case "rate_limit_exceeded", "rate_limit_error", "usage_limit_reached", "insufficient_quota":
			found = true
		default:
			return false
		}
	}
	return found
}

func smartWSIsExplicitTemporaryRejection(payload []byte) bool {
	if !gjson.ValidBytes(payload) || smartWSResponseID(payload) != "" || smartWSPayloadHasWork(payload) || isOpenAIContentPolicyRejection(payload) {
		return false
	}
	// Generic server_error text alone cannot prove a pre-execution rejection.
	// Require an explicit server status or a provider's capacity code.
	for _, path := range []string{"error.status_code", "error.status", "response.error.status_code", "response.error.status", "status_code", "status"} {
		status := gjson.GetBytes(payload, path)
		if status.Int() >= 500 && status.Int() <= 599 && status.Float() == float64(status.Int()) {
			return true
		}
	}
	for _, path := range []string{"error.code", "response.error.code"} {
		switch gjson.GetBytes(payload, path).String() {
		case "server_is_overloaded", "slow_down", "service_unavailable", "service_unavailable_error":
			return true
		}
	}
	return false
}

// Call before delivering the error. HTTP and initial native turns hand the
// rejection to the handler; later native turns retry only on the same socket.
func (s *OpenAIGatewayService) smartWSRetryableRejection(c *gin.Context, account *Account, status int, headers http.Header, payload []byte, handshake bool, model string) *UpstreamFailoverError {
	if c == nil || account == nil || account.Platform != PlatformOpenAI || (status != http.StatusTooManyRequests && (status < 500 || status > 599)) ||
		SmartWSFailureDelivered(c) || c.Request.Context().Err() != nil {
		return nil
	}
	if GetOpsCyberPolicy(c) != nil || isOpenAIContentPolicyRejection(payload) {
		return nil
	}
	value, _ := c.Get(smartWSDispatchKey)
	d, _ := value.(*smartWSDispatch)
	if handshake {
		if smartWSHasDispatched(c) || (d != nil && d.accepted.Load()) {
			return nil
		}
		// A bare HTTP 429 is pre-dispatch. Structured errors must also explicitly
		// identify rate limiting; never reinterpret an unknown refusal as quota.
		if status == http.StatusTooManyRequests && (gjson.GetBytes(payload, "error").Exists() || gjson.GetBytes(payload, "response.error").Exists()) && !smartWSIsExplicitRateLimit(payload) {
			return nil
		}
	} else if d == nil || d.accepted.Load() || (!smartWSIsExplicitRateLimit(payload) && !smartWSIsExplicitTemporaryRejection(payload)) {
		return nil
	}
	if smartWSResponseID(payload) != "" || smartWSPayloadHasWork(payload) {
		return nil
	}
	code, typ, message := parseOpenAIWSErrorEventFields(payload)
	if status >= 500 {
		if message == "" {
			message = extractUpstreamErrorMessage(payload)
		}
		if !s.shouldFailoverOpenAIUpstreamResponse(account, status, message, payload) {
			return nil
		}
		if d != nil {
			d.state.Store(smartWSRejected)
		}
		setOpsUpstreamError(c, status, sanitizeUpstreamErrorMessage(message), "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, Platform: account.Platform,
			UpstreamStatusCode: status, Kind: "ws_upstream_error", Message: sanitizeUpstreamErrorMessage(message)})
		shouldDisable := false
		classification := classifySmartWSHandshake(status, payload)
		if !handshake || classification.accountFailure {
			shouldDisable = s.handleOpenAIAccountUpstreamError(c.Request.Context(), account, status, headers, payload, model)
		}
		failure := s.newOpenAIAccountFailoverError(account, status, headers, payload, message, shouldDisable, !shouldDisable)
		failure.PreserveUpstreamError = true
		if handshake {
			classification.annotate(failure)
		}
		return failure
	}
	if handshake && code == "" && typ == "" {
		code, typ = "rate_limit_exceeded", "rate_limit_error"
	}
	if d != nil {
		d.state.Store(smartWSRejected)
	}
	s.persistOpenAIWSRateLimitSignal(c.Request.Context(), account, headers, payload, code, typ, message, model)
	return s.newOpenAIWSRateLimitFailoverError(account, headers, payload, message)
}
