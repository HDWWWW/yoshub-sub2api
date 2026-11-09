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

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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
	payload, tail, request []byte
	headers                http.Header
	started                time.Time
	httpFallback           bool
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
func readSmartWSFailedTail(ctx context.Context, lease *openAIWSConnLease, first []byte, responseID string) []byte {
	if gjson.GetBytes(first, "type").String() != "error" {
		return nil
	}
	if id := smartWSResponseID(first); id != "" {
		responseID = id
	}
	tailCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	for i := 0; i < 8; i++ {
		payload, err := lease.ReadMessageWithContextTimeout(tailCtx, 200*time.Millisecond)
		if err != nil || !gjson.ValidBytes(payload) {
			return nil
		}
		typ := gjson.GetBytes(payload, "type").String()
		id := smartWSResponseID(payload)
		if responseID != "" && id != "" && id != responseID {
			return nil
		}
		if typ == "response.failed" {
			return payload
		}
		if typ == "response.created" || isOpenAIWSTerminalEvent(typ) {
			return nil
		}
	}
	return nil
}

// Capture at the adapter boundary: buffered and streaming converters can
// return before accounting, but must not lose an observed failure's usage.
func rememberSmartWSHTTPFailure(c *gin.Context, headers http.Header, request, payload, tail []byte, started time.Time) {
	c.Set(smartWSHTTPFailureKey, &smartWSHTTPFailure{payload: append([]byte(nil), payload...),
		tail: append([]byte(nil), tail...), request: request, headers: headers.Clone(), started: started})
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
		return result, s.writeSmartWSHTTPError(c, account, response, failure.payload, gjson.GetBytes(failure.request, "model").String())
	}
	// Existing formatters own the wire format. This branch covers an early
	// failure after metadata/keepalive but before a formatter emitted the error.
	payload := failure.payload
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
type smartWSDispatch struct{ state atomic.Int32 }

const (
	smartWSNotSent int32 = iota
	smartWSMayBeSent
	smartWSReceived
	smartWSTerminal
)

func beginSmartWSDispatch(c *gin.Context) *smartWSDispatch {
	d := &smartWSDispatch{}
	if c != nil {
		c.Set(smartWSDispatchKey, d)
		c.Set(smartWSFailureDeliveredKey, false)
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

// Generic failover formatting hides whether generation may already have begun.
// Deliver an explicit non-replay outcome while retaining any observed usage.
func finalizeSmartWSUncertainFailure(c *gin.Context, result *OpenAIForwardResult, err error) (*OpenAIForwardResult, error) {
	if err == nil || !smartWSOutcomeUncertain(c) || SmartWSFailureDelivered(c) ||
		c.Request.Context().Err() != nil || (result != nil && result.ClientDisconnect) {
		return result, err
	}
	payload, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": smartWSUncertainCode, "message": smartWSUncertainMessage}})
	setOpsUpstreamError(c, http.StatusBadGateway, smartWSUncertainMessage, "")
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
// and account retry paths cannot accidentally turn an uncertain dispatch into
// an automatic replay. It intentionally retains observed usage in the result.
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
	payload = s.redactAgentIdentitySensitiveBody(c.Request.Context(), account, payload)
	if isOpenAICyberAccessDenied(resp.StatusCode, payload) {
		return writeOpenAICyberAccessDenied(c, account, resp, payload, nil)
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
	isPolicy := markOpenAICyberPolicyEvent(c, payload, resp.StatusCode, &usage)
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
	contentType := "application/json"
	if !json.Valid(payload) {
		contentType = "text/plain; charset=utf-8"
	}
	MarkResponseCommitted(c)
	downstreamStatus := resp.StatusCode
	if isPolicy {
		downstreamStatus = openAISafetyPolicyHTTPStatus(downstreamStatus)
	}
	if !isPolicy || !writeOpenAICompactSSEBridge(c, downstreamStatus, payload) {
		c.Data(downstreamStatus, contentType, payload)
	}
	c.Set(smartWSFailureDeliveredKey, true)
	failure := smartWSStopFailure(resp.StatusCode, payload, resp.Header)
	if isHandshake {
		handshake.annotate(failure)
	}
	return failure
}
