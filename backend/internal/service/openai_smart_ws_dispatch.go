package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/smartws"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// Some failures carry billable partial usage even as the first event. Keep it
// for the existing accounting hooks instead of discarding it with the error.
func smartWSHTTPFailureResult(resp *http.Response, payload, request []byte, model, upstreamModel string, stream bool, started time.Time, tail ...[]byte) *OpenAIForwardResult {
	var usage OpenAIUsage
	observer := &upstreamResponseModelObserver{}
	typ, id, hasUsage := "", "", false
	observe := func(frame []byte) {
		if len(frame) == 0 {
			return
		}
		hasUsage = hasUsage || gjson.GetBytes(frame, "response.usage").IsObject() || gjson.GetBytes(frame, "usage").IsObject()
		parseOpenAIWSResponseUsageFromCompletedEvent(frame, &usage)
		typ = gjson.GetBytes(frame, "type").String()
		if typ == "" || typ == "response" {
			if status := gjson.GetBytes(frame, "status").String(); status != "" {
				typ = "response." + status
			}
		}
		observer.ObserveOpenAI(frame, typ)
		if responseID := smartWSResponseID(frame); responseID != "" {
			id = responseID
		}
	}
	observe(payload)
	for _, frame := range tail {
		observe(frame)
	}
	if !hasUsage {
		return nil
	}
	return &OpenAIForwardResult{
		RequestID: id, ResponseID: id, Usage: usage, Model: model, UpstreamModel: upstreamModel,
		UpstreamResponseModel: observer.Model(), UpstreamResponseModelConflict: observer.Conflict(),
		UpstreamResponseServiceTier: observer.ServiceTier(),
		ServiceTier:                 resolvedOpenAIUpstreamServiceTierFromObserver(observer, extractOpenAIServiceTierFromBody(request)),
		ReasoningEffort:             extractOpenAIReasoningEffortFromBody(request, upstreamModel, model),
		RequestedReasoningEffort:    CanonicalRequestedReasoningEffort(request, model, upstreamModel),
		Stream:                      stream, OpenAIWSMode: isOpenAIHTTPWSResponse(resp), UpstreamTerminalEvent: typ,
		ResponseHeaders: resp.Header.Clone(), Duration: time.Since(started),
	}
}

// Request-level policy failures must never disable a credential. Semantic 429
// events still retain the upstream reset time, even though replay is forbidden.
func (s *OpenAIGatewayService) handleSmartWSFailureSideEffects(ctx context.Context, c *gin.Context, account *Account, model string, headers http.Header, payload []byte) bool {
	var usage OpenAIUsage
	parseOpenAIWSResponseUsageFromCompletedEvent(payload, &usage)
	if markOpenAICyberPolicyEvent(c, payload, http.StatusOK, &usage) {
		return true
	}
	if GetOpsCyberPolicy(c) != nil || isOpenAIContentPolicyRejection(payload) {
		return true
	}
	code, typ, message := parseOpenAIWSErrorEventFields(payload)
	if isOpenAIWSRateLimitError(code, typ, message) {
		s.persistOpenAIWSRateLimitSignal(ctx, account, headers, payload, code, typ, message, model)
		return true
	}
	return s.handleOpenAIWSFailureAccountSideEffects(ctx, account, model, headers, payload)
}

// Preserve the first policy error as evidence, but enrich its accounting from
// the same turn's later failed frame. Replace the mark instead of mutating a
// pointer that a handler may retain; empty usage cannot erase observed tokens.
func mergeSmartWSCyberPolicyUsage(c *gin.Context, usage *OpenAIUsage) {
	mark := GetOpsCyberPolicy(c)
	if mark == nil || usage == nil {
		return
	}
	updated := *mark
	updated.UpstreamInTok = max(updated.UpstreamInTok, usage.InputTokens)
	updated.UpstreamOutTok = max(updated.UpstreamOutTok, usage.OutputTokens)
	c.Set(opsCyberPolicyKey, &updated)
}

const smartWSDispatchKey = "openai_smart_ws_dispatch"
const smartWSFailureDeliveredKey = "openai_smart_ws_failure_delivered"
const smartWSHTTPFailureKey = "openai_smart_ws_http_failure"
const smartWSHTTPPartialKey = "openai_smart_ws_http_partial"

type smartWSHTTPFailure struct {
	payload, tail, request, wire []byte
	headers                      http.Header
	started                      time.Time
	httpFallback                 bool
}

func smartWSResponseID(payload []byte) string {
	if id := gjson.GetBytes(payload, "response.id").String(); id != "" {
		return id
	}
	if gjson.GetBytes(payload, "status").Exists() {
		if id := gjson.GetBytes(payload, "id").String(); id != "" {
			return id
		}
	}
	return gjson.GetBytes(payload, "response_id").String()
}

// A bare error may be followed by the authoritative failed usage. Read only a
// bounded tail on the already failed lease; never dispatch or wait a full turn.
func readSmartWSFailedTail(ctx context.Context, lease *openAIWSConnLease, first []byte, responseID string, observe func([]byte), tailReaders ...func(context.Context, time.Duration) ([]byte, error)) (lastError []byte) {
	if gjson.GetBytes(first, "type").String() != "error" {
		return nil
	}
	if id := smartWSResponseID(first); id != "" {
		responseID = id
	}
	tailCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	read := lease.ReadMessageWithContextTimeout
	if len(tailReaders) > 0 {
		read = tailReaders[0]
	}
	for i := 0; i < 8; i++ {
		payload, err := read(tailCtx, 200*time.Millisecond)
		if err != nil || !gjson.ValidBytes(payload) {
			return lastError
		}
		typ := gjson.GetBytes(payload, "type").String()
		id := smartWSResponseID(payload)
		if responseID != "" && id != "" && id != responseID {
			return lastError
		}
		observe(payload)
		if typ == "response.failed" {
			return payload
		}
		if typ == "error" {
			lastError = payload
		}
		if typ == "response.created" || isOpenAIWSTerminalEvent(typ) {
			return lastError
		}
	}
	return lastError
}

// Downstream relays decide whether to replay at the first error. The pool has
// already checked ownership of tail. Prefer a billable terminal from this turn
// and keep the original failure separately for side effects and accounting.
func smartWSBillableFailureFrame(first, tail []byte, responseID, model string, createdAt int64) []byte {
	if isOpenAIContentPolicyRejection(first) || gjson.GetBytes(first, "type").Str != "error" {
		return first
	}
	if gjson.GetBytes(tail, "type").Str == "response.failed" {
		if usage, ok := extractOpenAIUsageFromJSONBytesOrdered(tail, true); ok && openAIUsageHasTokens(&usage) && gjson.GetBytes(tail, "response.usage").IsObject() {
			return tail
		}
	}
	// Some providers report usage on the bare error itself. Responses clients
	// consume response.usage on a failed terminal. Keep ordinary 4xx unchanged.
	status := openAIHTTPWSEventErrorStatus(first)
	usage, ok := extractOpenAIUsageFromJSONBytes(first)
	if (status != http.StatusTooManyRequests && status < 500) || !ok || !openAIUsageHasTokens(&usage) {
		return first
	}
	u := gjson.GetBytes(first, "usage")
	if !u.IsObject() {
		u = gjson.GetBytes(first, "response.usage")
	}
	if id := smartWSResponseID(first); id != "" {
		responseID = id
	}
	if !u.IsObject() {
		return first
	}
	frame := buildOpenAIResponseFailedPayload(responseID, model, createdAt, first, "")
	frame, _ = sjson.SetRawBytes(frame, "response.usage", []byte(u.Raw))
	frame, _ = sjson.SetBytes(frame, "response.error.status_code", status)
	return frame
}

// Capture at the adapter boundary: buffered and streaming converters can
// return before accounting, but must not lose an observed failure's usage.
func rememberSmartWSHTTPFailure(c *gin.Context, headers http.Header, request, payload, tail, wire []byte, started time.Time) {
	c.Set(smartWSHTTPFailureKey, &smartWSHTTPFailure{payload: append([]byte(nil), payload...),
		tail: append([]byte(nil), tail...), request: request, headers: headers.Clone(), started: started, wire: wire})
}

// Keep observed usage at the transport boundary, including events a formatter
// does not retain. Only the same response may update the immutable snapshot;
// nonzero terminal usage remains authoritative over earlier observations.
func rememberSmartWSHTTPUsage(c *gin.Context, headers http.Header, request, payload []byte, started time.Time, wsMode bool) {
	if c == nil || (!gjson.GetBytes(payload, "usage").IsObject() && !gjson.GetBytes(payload, "response.usage").IsObject()) {
		return
	}
	model := gjson.GetBytes(request, "model").String()
	observed := smartWSHTTPFailureResult(&http.Response{Header: headers}, payload, request, model, model,
		gjson.GetBytes(request, "stream").Bool(), started)
	if observed == nil {
		return
	}
	if value, ok := c.Get(smartWSHTTPPartialKey); ok {
		if previous, ok := value.(*OpenAIForwardResult); ok && previous != nil {
			if previous.ResponseID != "" && observed.ResponseID != "" && previous.ResponseID != observed.ResponseID {
				return
			}
			usage := previous.Usage
			parseOpenAIWSResponseUsageFromCompletedEvent(payload, &usage)
			observed.Usage = usage
			if observed.ResponseID == "" {
				observed.ResponseID, observed.RequestID = previous.ResponseID, previous.RequestID
			}
		}
	}
	observed.OpenAIWSMode = wsMode
	c.Set(smartWSHTTPPartialKey, observed)
}

func (s *OpenAIGatewayService) finalizeSmartWSHTTPFailure(c *gin.Context, account *Account, request []byte, result *OpenAIForwardResult, err error) (*OpenAIForwardResult, error) {
	if c == nil {
		return result, err
	}
	if value, ok := c.Get(smartWSDispatchKey); ok {
		if d, ok := value.(*smartWSDispatch); ok && d.state.Load() == smartWSRejected {
			return result, err
		}
	}
	v, _ := c.Get(smartWSHTTPFailureKey)
	failure, _ := v.(*smartWSHTTPFailure)
	if failure == nil {
		if smartWSOutcomeUncertain(c) {
			v, _ := c.Get(smartWSHTTPPartialKey)
			if observed, ok := v.(*OpenAIForwardResult); ok && observed != nil {
				if result == nil {
					copy := *observed
					copy.Model = gjson.GetBytes(request, "model").String()
					result = &copy
				} else {
					mergeOpenAIUsageNonZero(&result.Usage, observed.Usage)
				}
			}
		}
		return finalizeSmartWSUncertainFailure(c, result, err)
	}
	if !failure.httpFallback && !openAIStreamClientOutputStarted(c, false) {
		if retry := s.smartWSRetryableRejection(c, account, openAIHTTPWSEventErrorStatus(failure.payload), failure.headers, failure.payload, false,
			gjson.GetBytes(failure.request, "model").String()); retry != nil {
			return nil, retry
		}
	}
	// A pre-existing formatter can return a successful result for a JSON
	// body carrying status=failed. The observed upstream error owns the turn.
	if err == nil {
		err = smartWSStopFailure(openAIHTTPWSEventErrorStatus(failure.payload), failure.payload, failure.headers)
	}
	response := openAIHTTPWSErrorResponse(c.Request, openAIHTTPWSEventErrorStatus(failure.payload), failure.headers, failure.payload)
	if failure.httpFallback {
		// Retain HTTP billing attribution even though the fallback was selected
		// by SmartWS. Only the transport decision was WS before its rejection.
		response.Request = c.Request
	}
	observed := smartWSHTTPFailureResult(response, failure.payload, failure.request,
		gjson.GetBytes(request, "model").String(), gjson.GetBytes(failure.request, "model").String(),
		gjson.GetBytes(request, "stream").Bool(), failure.started, failure.tail)
	if observed == nil || !openAIUsageHasTokens(&observed.Usage) {
		value, _ := c.Get(smartWSHTTPPartialKey)
		partial, _ := value.(*OpenAIForwardResult)
		failureID := smartWSResponseID(failure.payload)
		if id := smartWSResponseID(failure.tail); id != "" {
			failureID = id
		}
		if partial != nil && (failureID == "" || partial.ResponseID == "" || failureID == partial.ResponseID) {
			if observed != nil {
				observed.Usage = partial.Usage
			} else {
				copy := *partial
				copy.Model = gjson.GetBytes(request, "model").String()
				copy.UpstreamTerminalEvent = "error"
				if gjson.GetBytes(failure.payload, "type").String() == "response.failed" ||
					gjson.GetBytes(failure.payload, "status").String() == "failed" || len(failure.tail) > 0 {
					copy.UpstreamTerminalEvent = "response.failed"
				}
				observed = &copy
			}
		}
	}
	if observed != nil {
		// The formatter may mark the bare error again. Apply the merged usage
		// after formatting so a subsequent empty error cannot erase it.
		defer func() {
			markOpenAICyberPolicyEvent(c, failure.payload, response.StatusCode, &observed.Usage)
			markOpenAICyberPolicyEvent(c, failure.tail, response.StatusCode, &observed.Usage)
			mergeSmartWSCyberPolicyUsage(c, &observed.Usage)
		}()
		if result == nil {
			result = observed
		} else {
			result.Usage = observed.Usage
			result.UpstreamTerminalEvent = observed.UpstreamTerminalEvent
			if observed.ResponseID != "" {
				result.ResponseID, result.RequestID = observed.ResponseID, observed.RequestID
			}
			if observed.ServiceTier != nil {
				result.ServiceTier = observed.ServiceTier
			}
			if observed.ReasoningEffort != nil {
				result.ReasoningEffort = observed.ReasoningEffort
			}
			if observed.UpstreamResponseModel != "" {
				result.UpstreamResponseModel = observed.UpstreamResponseModel
			}
			if observed.UpstreamResponseServiceTier != "" {
				result.UpstreamResponseServiceTier = observed.UpstreamResponseServiceTier
			}
		}
	}
	if c.Request.Context().Err() != nil || (result != nil && result.ClientDisconnect) {
		return result, err
	}
	if SmartWSFailureDelivered(c) {
		return result, err
	}
	if !c.Writer.Written() {
		return result, s.writeSmartWSHTTPError(c, account, response, failure.wire, gjson.GetBytes(failure.request, "model").String())
	}
	// Existing formatters own the wire format. This branch covers an early
	// failure after metadata/keepalive but before a formatter emitted the error.
	payload := s.redactAgentIdentitySensitiveBody(c.Request.Context(), account, failure.wire)
	payload = smartWSClientFrame(c, account, payload)
	if strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/chat/completions") {
		if nested := gjson.GetBytes(payload, "response.error"); nested.IsObject() {
			payload = []byte(`{"error":` + nested.Raw + `}`)
		}
	}
	failurePayload := failure.payload
	if len(failure.tail) > 0 {
		failurePayload = failure.tail
	}
	s.handleSmartWSFailureSideEffects(c.Request.Context(), c, account, gjson.GetBytes(failure.request, "model").String(), failure.headers, failurePayload)
	if _, writeErr := fmt.Fprintf(c.Writer, "data: %s\n\n", payload); writeErr != nil {
		if result != nil {
			result.ClientDisconnect = true
		}
		return result, writeErr
	}
	c.Writer.Flush()
	c.Set(smartWSFailureDeliveredKey, true)
	return result, smartWSStopFailure(response.StatusCode, failure.payload, failure.headers)
}

// SmartWSFailureDelivered means the exact upstream failure has already reached
// the client. The handler still records observed usage but must not append a
// second, generic error or schedule another account.
func SmartWSFailureDelivered(c *gin.Context) bool {
	return c != nil && c.GetBool(smartWSFailureDeliveredKey)
}

// State is set BEFORE the first write: a failed Write does not establish that
// zero bytes reached the server. Downstream visibility is not a replay proof.
type smartWSDispatch struct {
	state    atomic.Int32
	accepted atomic.Bool
	policy   atomic.Pointer[[]byte]
	// Set at HTTP dispatch; observations stay sticky until the next attempt.
	httpPreOutputRecovery bool
	transportFailure      atomic.Bool
	replayUnsafe          atomic.Bool
	readTimeout           time.Duration
	readFailure           atomic.Pointer[smartws.SentError]
}

func (d *smartWSDispatch) observeReadError(err error) {
	var sent *smartws.SentError
	if d != nil && errors.As(err, &sent) {
		d.readFailure.CompareAndSwap(nil, sent)
		if sent.TransportFailure {
			d.transportFailure.Store(true)
		}
	}
}

const (
	smartWSNotSent int32 = iota
	smartWSMayBeSent
	smartWSReceived
	smartWSTerminal
)

// The failed attempt has been handed back to the HTTP retry owner. Do not let
// its dispatch state rewrite errors raised before the next attempt dispatches.
const smartWSHTTPRetryable int32 = -2

func beginSmartWSDispatch(c *gin.Context) *smartWSDispatch {
	d := &smartWSDispatch{}
	if c != nil {
		c.Set(smartWSDispatchKey, d)
		c.Set(smartWSFailureDeliveredKey, false)
		c.Set(smartWSHTTPFailureKey, nil)
		c.Set(smartWSHTTPPartialKey, nil)
	}
	return d
}

func smartWSHasDispatched(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, _ := c.Get(smartWSDispatchKey)
	d, _ := v.(*smartWSDispatch)
	return d != nil && d.state.Load() >= smartWSMayBeSent
}

const smartWSUncertainCode = "smart_ws_dispatch_uncertain"
const smartWSUncertainMessage = "Upstream connection ended before a terminal result; the request may have been executed and was not replayed."

func smartWSOutcomeUncertain(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, _ := c.Get(smartWSDispatchKey)
	d, _ := v.(*smartWSDispatch)
	if d == nil {
		return false
	}
	state := d.state.Load()
	return state >= smartWSMayBeSent && state < smartWSTerminal
}

// A transport interruption before output is recoverable by the HTTP handler's
// existing bounded retry policy. This cannot prove that upstream did no work:
// never extend it to observed usage/tools, explicit refusals or continuations.
func smartWSCanRetryHTTPBeforeOutput(c *gin.Context) bool {
	if !smartWSOutcomeUncertain(c) || SmartWSFailureDelivered(c) || c.Request.Context().Err() != nil ||
		GetOpsCyberPolicy(c) != nil || openAIStreamClientOutputStarted(c, false) {
		return false
	}
	v, _ := c.Get(smartWSDispatchKey)
	d, _ := v.(*smartWSDispatch)
	return d != nil && d.httpPreOutputRecovery && d.transportFailure.Load() && !d.replayUnsafe.Load() && d.policy.Load() == nil
}

// Recover eligible pre-output transport failures without writing to the client.
// Other uncertain outcomes remain explicit and retain any observed usage.
func finalizeSmartWSUncertainFailure(c *gin.Context, result *OpenAIForwardResult, err error) (*OpenAIForwardResult, error) {
	if err == nil || !smartWSOutcomeUncertain(c) || c.Request.Context().Err() != nil || (result != nil && result.ClientDisconnect) {
		return result, err
	}
	retry := smartWSCanRetryHTTPBeforeOutput(c) && (result == nil || (!openAIUsageHasTokens(&result.Usage) && result.ImageCount == 0))
	v, _ := c.Get(smartWSDispatchKey)
	d, ok := v.(*smartWSDispatch)
	if !ok || d == nil {
		return result, err
	}
	detail := ""
	if failure := d.readFailure.Load(); failure != nil {
		fields := gin.H{"reason": failure.Reason, "close_code": failure.CloseCode,
			"read_timeout_seconds": d.readTimeout.Seconds(), "retry_before_output": retry,
			"dispatch_state": d.state.Load(), "replay_unsafe": d.replayUnsafe.Load()}
		encoded, _ := json.Marshal(fields)
		detail = string(encoded)
		// A streaming formatter may already have delivered the public error.
		// Persist its cause before the no-second-error guard below.
		c.Set(OpsUpstreamErrorDetailKey, detail)
		logger.FromContext(c.Request.Context()).Warn("smart_ws.stream_failure",
			zap.String("reason", failure.Reason), zap.Int("close_code", failure.CloseCode),
			zap.Float64("read_timeout_seconds", d.readTimeout.Seconds()), zap.Bool("retry_before_output", retry),
			zap.Int32("dispatch_state", d.state.Load()), zap.Bool("replay_unsafe", d.replayUnsafe.Load()))
	}
	if SmartWSFailureDelivered(c) {
		return result, err
	}
	if retry {
		const message = "Upstream connection ended before model output"
		payload, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": "upstream_stream_disconnected", "message": message}})
		setOpsUpstreamError(c, http.StatusBadGateway, message, detail)
		failure := &UpstreamFailoverError{}
		var previous *UpstreamFailoverError
		if errors.As(err, &previous) {
			*failure = *previous
		}
		failure.StatusCode, failure.ResponseBody = http.StatusBadGateway, payload
		failure.NextAccountAction, failure.RetryableOnSameAccount = NextAccountRetry, true
		failure.PreserveUpstreamError = true
		d.state.Store(smartWSHTTPRetryable)
		return result, failure
	}
	payload, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": smartWSUncertainCode, "message": smartWSUncertainMessage}})
	setOpsUpstreamError(c, http.StatusBadGateway, smartWSUncertainMessage, detail)
	if !c.Writer.Written() {
		c.Data(http.StatusBadGateway, "application/json", payload)
	} else {
		if !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/chat/completions") {
			payload, _ = json.Marshal(gin.H{"type": "error", "sequence_number": 0, "code": smartWSUncertainCode, "message": smartWSUncertainMessage, "param": nil})
		}
		if _, writeErr := fmt.Fprintf(c.Writer, "data: %s\n\n", payload); writeErr != nil {
			if result != nil {
				result.ClientDisconnect = true
			}
			return result, writeErr
		}
		c.Writer.Flush()
	}
	MarkResponseCommitted(c)
	c.Set(smartWSFailureDeliveredKey, true)
	return result, smartWSStopFailure(http.StatusBadGateway, payload, nil)
}

// Also applied at the public forward boundary, so stream, timeout, conversion
// and account retry paths cannot replay an uncertain dispatch unless the HTTP
// pre-output policy explicitly handed it back. Observed usage stays in result.
func stopSmartWSReplay(c *gin.Context, err error) error {
	if err == nil || !smartWSHasDispatched(c) {
		return err
	}
	var failure *UpstreamFailoverError
	if errors.As(err, &failure) {
		copy := *failure
		copy.NextAccountAction = NextAccountStop
		copy.RetryableOnSameAccount = false
		return &copy
	}
	return err
}

type smartWSDispatchError struct{ cause error }

func (e *smartWSDispatchError) Error() string {
	return "upstream dispatch outcome is unknown; request was not replayed"
}
func (e *smartWSDispatchError) Unwrap() error { return e.cause }

func smartWSUncertainError(err error) error {
	if err == nil {
		return nil
	}
	return &smartWSDispatchError{cause: err}
}

func smartWSStopFailure(status int, body []byte, headers http.Header) *UpstreamFailoverError {
	failure := &UpstreamFailoverError{StatusCode: status, ResponseBody: body, ResponseHeaders: headers, NextAccountAction: NextAccountStop}
	if hit, code, _ := detectOpenAICyberPolicy(body); hit {
		failure.Scope = GatewayFailureScopeRequest
		failure.Reason = GatewayFailureReason(code)
	}
	return failure
}

func (s *OpenAIGatewayService) writeSmartWSHTTPError(c *gin.Context, account *Account, resp *http.Response, payload []byte, upstreamModel ...string) error {
	if mark := GetOpsCyberPolicy(c); mark != nil && mark.Body != "" {
		// A later authoritative policy refusal owns the failure, even if the
		// first envelope looked like rate limiting.
		payload = []byte(mark.Body)
	}
	payload = openAIPolicyHTTPErrorPayload(payload)
	payload = s.redactAgentIdentitySensitiveBody(c.Request.Context(), account, payload)
	if isOpenAICyberAccessDenied(resp.StatusCode, payload) {
		var writeError compatErrorWriter
		if stopSmartWSKeepaliveAndCheckSSE(c) {
			writeError = func(c *gin.Context, status int, _, _ string) {
				if !writeOpenAICompactSSEBridge(c, status, payload) {
					writeSmartWSCommittedSSEError(c, status, payload)
				}
			}
		}
		err := writeOpenAICyberAccessDenied(c, account, resp, payload, writeError)
		c.Set(smartWSFailureDeliveredKey, true) // The entitlement formatter also owns its uncommitted JSON reply.
		return err
	}
	message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(payload))
	setOpsUpstreamError(c, resp.StatusCode, message, "")
	event := OpsUpstreamErrorEvent{AccountID: account.ID, Platform: account.Platform, UpstreamStatusCode: resp.StatusCode, Kind: "ws_upstream_error", Message: message}
	event.ProxyID, event.ProxyName = opsUpstreamWSProxyAttribution(account)
	var handshake smartWSHandshakeFailure
	var isHandshake bool
	if resp.Request != nil {
		handshake, isHandshake = resp.Request.Context().Value(smartWSHandshakeKey{}).(smartWSHandshakeFailure)
	}
	if (isHandshake || isOpenAIHTTPWSResponse(resp)) && !openAIStreamClientOutputStarted(c, false) {
		model := ""
		if len(upstreamModel) > 0 {
			model = upstreamModel[0]
		}
		if retry := s.smartWSRetryableRejection(c, account, resp.StatusCode, resp.Header, payload, isHandshake, model); retry != nil {
			retry.PreserveUpstreamError = true
			return retry
		}
	}
	// A non-successful HTTP fallback or handshake is an explicit rejection.
	// Return it to the account retry owner before committing the downstream
	// response; the transport's dispatch guard must not consume that budget.
	if (!isOpenAIHTTPWSResponse(resp) || isHandshake) &&
		(resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && !openAIStreamClientOutputStarted(c, false) &&
		!SmartWSFailureDelivered(c) && c.Request.Context().Err() == nil &&
		!smartWSPayloadHasWork(payload) && smartWSResponseID(payload) == "" &&
		s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, message, payload) {
		appendOpsUpstreamError(c, event)
		shouldDisable := false
		if !isHandshake || handshake.accountFailure {
			shouldDisable = s.handleOpenAIAccountUpstreamError(c.Request.Context(), account, resp.StatusCode, resp.Header, payload, upstreamModel...)
		}
		retrySameAccount := !shouldDisable && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 ||
			(account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode)))
		failure := s.newOpenAIAccountFailoverError(account, resp.StatusCode, resp.Header, payload, message, shouldDisable, retrySameAccount)
		failure.PreserveUpstreamError = true
		if isHandshake {
			handshake.annotate(failure)
		}
		if value, ok := c.Get(smartWSDispatchKey); ok {
			if dispatch, ok := value.(*smartWSDispatch); ok {
				dispatch.state.Store(smartWSRejected)
			}
		}
		return failure
	}
	if isHandshake {
		event.Stage, event.Scope, event.Reason = "ws_handshake", "upstream_transport", handshake.category
		if handshake.accountFailure {
			event.Scope = "account"
		}
		if resp.Request.URL != nil {
			// Deliberately exclude userinfo, query, fragment and response headers.
			event.UpstreamURL = resp.Request.URL.Host + resp.Request.URL.EscapedPath()
		}
	}
	appendOpsUpstreamError(c, event)
	var usage OpenAIUsage
	parseOpenAIWSResponseUsageFromCompletedEvent(payload, &usage)
	markOpenAICyberPolicyEvent(c, payload, resp.StatusCode, &usage)
	isPolicy := GetOpsCyberPolicy(c) != nil || isOpenAIContentPolicyRejection(payload)
	if !isPolicy {
		if !isOpenAIHTTPWSResponse(resp) {
			// A safe pre-dispatch fallback may have produced an actual HTTP
			// business response. Preserve its original account policy.
			_ = s.handleOpenAIAccountUpstreamError(c.Request.Context(), account, resp.StatusCode, resp.Header, payload, upstreamModel...)
		} else if isHandshake {
			if handshake.accountFailure {
				_ = s.handleOpenAIAccountUpstreamError(c.Request.Context(), account, resp.StatusCode, resp.Header, payload, upstreamModel...)
			}
		} else {
			model := ""
			if len(upstreamModel) > 0 {
				model = upstreamModel[0]
			}
			s.handleSmartWSFailureSideEffects(c.Request.Context(), c, account, model, resp.Header, payload)
		}
	}
	for _, key := range []string{"Retry-After", "X-Request-Id"} {
		if v := resp.Header.Get(key); v != "" {
			c.Header(key, v)
		}
	}
	downstreamStatus, clientPayload := smartWSClientError(c, account, resp.StatusCode, payload)
	contentType := "application/json"
	if !json.Valid(clientPayload) {
		contentType = "text/plain; charset=utf-8"
	}
	MarkResponseCommitted(c)
	if isPolicy {
		downstreamStatus = openAISafetyPolicyHTTPStatus(downstreamStatus)
	}
	// A slot-wait ping can commit SSE before SmartWS sees its first frame.
	// Keep the compact policy writer first; otherwise use the protocol that
	// is already on the wire, not the upstream response's content type.
	if !isPolicy || !writeOpenAICompactSSEBridge(c, downstreamStatus, clientPayload) {
		sse := stopSmartWSKeepaliveAndCheckSSE(c)
		// A caller requesting streamed Responses needs the billable terminal even
		// before its first byte. An HTTP 503 would bypass its SSE usage parser.
		if !sse && !c.Writer.Written() && !isPolicy && (resp.StatusCode == 429 || resp.StatusCode >= 500) &&
			openAIUsageHasTokens(&usage) && strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
			v, _ := c.Get(smartWSHTTPFailureKey)
			if f, ok := v.(*smartWSHTTPFailure); ok && f != nil && !f.httpFallback && gjson.GetBytes(f.request, "stream").Bool() {
				setSSEHeaders(c)
				sse = true
			}
		}
		if sse {
			writeSmartWSCommittedSSEError(c, downstreamStatus, clientPayload)
		} else {
			c.Data(downstreamStatus, contentType, clientPayload)
		}
	}
	c.Set(smartWSFailureDeliveredKey, true)
	failure := smartWSStopFailure(resp.StatusCode, payload, resp.Header)
	if isHandshake {
		handshake.annotate(failure)
	}
	return failure
}

func stopSmartWSKeepaliveAndCheckSSE(c *gin.Context) bool {
	StopOpenAICompactSSEKeepaliveCommitted(c)
	return c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream")
}

func writeSmartWSCommittedSSEError(c *gin.Context, status int, payload []byte) {
	errType, code, message := OpenAIUpstreamErrorFields(status, payload)
	var frame string
	switch {
	case strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/chat/completions"):
		data, _ := json.Marshal(gin.H{"error": gin.H{"type": errType, "code": code, "message": message, "status_code": status}})
		frame = "data: " + string(data) + "\n\n"
	case gjson.GetBytes(payload, "type").Str == "response.failed" && gjson.GetBytes(payload, "response").IsObject() && json.Valid(payload):
		// Preserve the actual terminal (including ID and usage); SSE data is one line.
		data, _ := json.Marshal(json.RawMessage(payload))
		frame = "event: response.failed\ndata: " + string(data) + "\n\n"
	default:
		frame = buildOpenAIResponseFailedSSE(smartWSResponseID(payload), "", 0, payload, message)
	}
	if _, err := fmt.Fprint(c.Writer, frame); err == nil {
		c.Writer.Flush()
	} else {
		_ = c.Error(err) // A partial terminal must not be followed by another error.
	}
}
