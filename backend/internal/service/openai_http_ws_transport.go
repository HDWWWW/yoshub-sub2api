package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIHTTPWSExecutionScopeKey = "openai_http_ws_execution_scope"

type openAIHTTPWSTransportKey struct{}

func isOpenAIHTTPWSResponse(resp *http.Response) bool {
	return resp != nil && resp.Request != nil && resp.Request.Context().Value(openAIHTTPWSTransportKey{}) == true
}

// doOpenAIHTTPOverWS changes only the upstream transport. The existing HTTP
// response handlers still own downstream formatting, happy-first-token signals,
// real TTFT, failure handling and billing. No HTTP retry is hidden in this adapter.
func (s *OpenAIGatewayService) doOpenAIHTTPOverWS(c *gin.Context, req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	originalBody := req.Body
	defer originalBody.Close()
	observation := s.beginSmartWSObservation(c)
	dispatch := beginSmartWSDispatch(c)
	requestLimit := s.openAIWSRequestLimit()
	raw, err := io.ReadAll(io.LimitReader(originalBody, requestLimit+1))
	if err != nil {
		return nil, err
	}
	// Keep the entire original body available even when the WS limit is crossed.
	// Falling back is safe here: no response.create has been written.
	fallback := func() (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		previousID := gjson.GetBytes(raw, "previous_response_id").String()
		observationRequest := raw
		if int64(len(raw)) > requestLimit {
			// The identifier may occur after a large input. Finalized gateway
			// requests have GetBody backed by the already bounded HTTP payload;
			// inspect that complete JSON without consuming the fallback body.
			if req.GetBody == nil {
				return nil, errors.New("cannot verify continuation before oversized request fallback")
			}
			copy, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			var continuation struct {
				PreviousResponseID string          `json:"previous_response_id"`
				Model              string          `json:"model"`
				Stream             bool            `json:"stream"`
				Reasoning          json.RawMessage `json:"reasoning,omitempty"`
				ServiceTier        json.RawMessage `json:"service_tier,omitempty"`
			}
			err = json.NewDecoder(copy).Decode(&continuation)
			_ = copy.Close()
			if err != nil {
				return nil, err
			}
			previousID = continuation.PreviousResponseID
			observationRequest, err = json.Marshal(continuation)
			if err != nil {
				return nil, err
			}
		}
		// A prior WS response can exist only on its originating connection.
		// Never silently move such a continuation to a different transport.
		if previousID != "" {
			observation.m.continuationUnavailable.Add(1)
			return openAIHTTPWSErrorResponse(req, http.StatusConflict, nil, []byte(`{"error":{"type":"invalid_request_error","code":"smart_ws_continuation_unavailable","message":"The upstream WebSocket continuation is unavailable; this request was not dispatched."}}`)), nil
		}
		observation.m.fallback.Add(1)
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), originalBody))
		observation.fallback = true
		observation.dispatched = true
		dispatch.state.Store(smartWSMayBeSent)
		// 307/308 would otherwise resend this POST inside http.Client.Do,
		// bypassing the dispatch state machine and account retry guard.
		req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
		req.GetBody = nil
		response, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return response, smartWSUncertainError(err)
		}
		dispatch.state.Store(smartWSReceived)
		if response != nil && response.StatusCode >= 300 {
			observation.terminal = "error"
			observation.errorObserved = true
			observation.m.upstreamError.Add(1)
			dispatch.state.Store(smartWSTerminal)
		}
		if response != nil && response.StatusCode >= 200 && response.StatusCode < 300 && response.Body != nil {
			lineLimit := int64(defaultMaxLineSize)
			if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
				lineLimit = int64(s.cfg.Gateway.MaxLineSize)
			}
			response.Body = newSmartWSFallbackBody(response.Body, response.Header, observationRequest, c, dispatch, observation, lineLimit, resolveUpstreamResponseReadLimit(s.cfg))
		}
		return response, nil
	}
	if int64(len(raw)) > requestLimit-1024 {
		return fallback()
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil || body == nil || !gjson.ValidBytes(raw) {
		return nil, errors.New("invalid Responses request for upstream WebSocket")
	}
	if background, _ := body["background"].(bool); background {
		return fallback()
	}
	payload := s.buildOpenAIWSCreatePayload(body, account)
	// Even a non-streaming downstream needs the terminal WS response collected
	// by the existing SSE-to-JSON handler.
	if account.UsesOpenAICodexProtocol() {
		payload["stream"] = true
	} else {
		delete(payload, "stream")
	}
	setOpenAIWSTurnMetadata(payload, req.Header.Get(openAIWSTurnMetadataHeader))
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode upstream WebSocket request: %w", err)
	}
	if int64(len(encoded)) >= requestLimit {
		return fallback()
	}

	wsURL := *req.URL
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	case "http":
		wsURL.Scheme = "ws"
	default:
		return nil, errors.New("unsupported upstream WebSocket URL scheme")
	}
	// Use the finalized HTTP headers/body so model routing, credential refresh,
	// account overrides and tenant-isolated identities stay in sync.
	headers := req.Header.Clone()
	mergeSmartWSBetaHeader(headers)
	headers.Del("Content-Length")
	stateStore := s.getOpenAIWSStateStore()
	groupID := getOpenAIGroupIDFromContext(c)
	scope := fmt.Sprintf("http:%d:%d:%s", account.ID, getAPIKeyIDFromContext(c), c.GetString(openAIHTTPWSExecutionScopeKey))
	previousID := openAIWSPayloadString(payload, "previous_response_id")
	preferred := ""
	if stateStore != nil {
		if previousID != "" {
			preferred, _ = stateStore.GetResponseConn(previousID)
		} else {
			preferred, _ = stateStore.GetSessionConn(groupID, scope)
		}
	}
	pool := s.getOpenAIWSConnPool()
	acquireRequest := openAIWSAcquireRequest{
		Account: account, WSURL: wsURL.String(), Headers: headers, ProxyURL: proxyURL,
		PreferredConnID: preferred, IsolationKey: scope, SkipPrewarm: true,
		ForcePreferredConn: previousID != "" && preferred != "" && s.isOpenAIWSStoreDisabledInRequest(body, account),
		HeadersFactory: func(ctx context.Context, headers http.Header) (http.Header, error) {
			return s.refreshOpenAIAgentIdentityHeaders(ctx, account, headers)
		},
	}
	if pool.rejections.active(smartWSRejectionDigest(acquireRequest), time.Now()) {
		observation.m.rejectionBypass.Add(1)
		c.Set("openai_ws_transport_reason", "handshake_rejection_cooldown")
		return fallback()
	}
	acquireCtx, cancelAcquire := context.WithTimeout(req.Context(), s.openAIWSAcquireTimeout())
	lease, err := pool.Acquire(acquireCtx, acquireRequest)
	cancelAcquire()
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		var dialErr *openAIWSDialError
		if errors.As(err, &dialErr) && dialErr.StatusCode >= 400 {
			pool.rememberSmartWSRejection(acquireRequest, dialErr)
			switch dialErr.StatusCode {
			case 404, 405, 426, 501:
				return fallback()
			}
			return smartWSHandshakeErrorResponse(req, dialErr), nil
		}
		return fallback()
	}
	owned := true
	defer func() {
		if owned {
			lease.MarkBroken()
			lease.Release()
		}
	}()
	SetOpsLatencyMs(c, OpsOpenAIWSConnPickMsKey, lease.ConnPickDuration().Milliseconds())
	SetOpsLatencyMs(c, OpsOpenAIWSQueueWaitMsKey, lease.QueueWaitDuration().Milliseconds())
	c.Set(OpsOpenAIWSConnReusedKey, lease.Reused())
	c.Set(OpsOpenAIWSConnIDKey, lease.ConnID())
	c.Set("openai_ws_transport_decision", string(OpenAIUpstreamTransportResponsesWebsocketV2))
	c.Set("openai_ws_transport_reason", "http_force_ws")
	observation.reused = lease.Reused()
	observation.dispatched = true
	dispatch.state.Store(smartWSMayBeSent)
	if err := lease.WriteJSONWithContextTimeout(req.Context(), json.RawMessage(encoded), s.openAIWSWriteTimeout()); err != nil {
		return nil, smartWSUncertainError(err)
	}
	// A successful handshake alone does not mean response.create was accepted.
	// Inspect the first event before opening downstream SSE; immediate errors
	// retain their status and payload, emit no happy signal, and are not replayed.
	first, err := lease.ReadMessageWithContextTimeout(req.Context(), s.openAIWSReadTimeout())
	if err != nil {
		return nil, smartWSUncertainError(err)
	}
	dispatch.state.Store(smartWSReceived)
	if !gjson.ValidBytes(first) || gjson.GetBytes(first, "type").String() == "" {
		return nil, smartWSUncertainError(errors.New("invalid first upstream WebSocket event"))
	}
	responseHeaders := lease.HandshakeHeaders()
	if responseHeaders == nil {
		responseHeaders = make(http.Header)
	}
	eventType := gjson.GetBytes(first, "type").String()
	rememberFailure := func(message, tail []byte) {
		rememberSmartWSHTTPFailure(c, responseHeaders, raw, message, tail, observation.start)
	}
	if eventType == "error" || eventType == "response.failed" || gjson.GetBytes(first, "response.status").String() == "failed" {
		dispatch.state.Store(smartWSTerminal)
		observation.event(first)
		tail := readSmartWSFailedTail(req.Context(), lease, first, "")
		if len(tail) > 0 {
			observation.event(tail)
		}
		rememberFailure(first, tail)
		status := openAIHTTPWSEventErrorStatus(first)
		return openAIHTTPWSErrorResponse(req, status, responseHeaders, first), nil
	}
	for _, key := range []string{"Upgrade", "Connection", "Sec-WebSocket-Accept", "Sec-WebSocket-Extensions", "Content-Length", "Content-Encoding"} {
		responseHeaders.Del(key)
	}
	responseHeaders.Set("Content-Type", "text/event-stream")
	responseHeaders.Set("Cache-Control", "no-cache")
	responseHeaders.Set("X-Accel-Buffering", "no")
	if responseID := strings.TrimSpace(gjson.GetBytes(first, "response.id").String()); responseID != "" {
		responseHeaders.Set("X-Request-Id", responseID)
	}
	wsBody := &openAIHTTPWSBody{
		dispatch: dispatch, observation: observation, onFailure: rememberFailure,
		ctx: req.Context(), lease: lease, first: first, readTimeout: s.openAIWSReadTimeout(),
		onComplete: func(responseID string) {
			if stateStore == nil {
				return
			}
			if responseID != "" {
				ttl := s.openAIWSResponseStickyTTL()
				logOpenAIWSBindResponseAccountWarn(groupID, account.ID, responseID, stateStore.BindResponseAccount(req.Context(), groupID, responseID, account.ID, ttl))
				stateStore.BindResponseConn(responseID, lease.ConnID(), ttl)
			}
			stateStore.BindSessionConn(groupID, scope, lease.ConnID(), s.openAIWSSessionStickyTTL())
		},
	}
	owned = false
	return &http.Response{
		StatusCode: http.StatusOK, Header: responseHeaders, Body: wsBody, ContentLength: -1,
		Request: req.WithContext(context.WithValue(req.Context(), openAIHTTPWSTransportKey{}, true)),
	}, nil
}

func openAIHTTPWSEventErrorStatus(payload []byte) int {
	for _, path := range openAIStreamErrorStatusPaths {
		if value := gjson.GetBytes(payload, path); value.Type == gjson.Number {
			if status := int(value.Int()); status >= 400 && status <= 599 {
				return status
			}
		}
	}
	return openAIStreamFailedEventSemanticStatus(payload, extractOpenAISSEErrorMessage(payload))
}

func openAIHTTPWSErrorResponse(req *http.Request, status int, headers http.Header, payload []byte) *http.Response {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if len(payload) == 0 {
		payload, _ = json.Marshal(map[string]any{"error": map[string]string{"type": "upstream_error", "message": http.StatusText(status)}})
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(payload)), Request: req.WithContext(context.WithValue(req.Context(), openAIHTTPWSTransportKey{}, true))}
}

// Reading is synchronous and backpressured. Close may run concurrently on a
// canceled request; only a terminal success can return a reusable connection.
type openAIHTTPWSBody struct {
	dispatch    *smartWSDispatch
	observation *smartWSObservation
	ctx         context.Context
	lease       *openAIWSConnLease
	readTimeout time.Duration
	first       []byte
	pending     []byte
	terminal    bool
	onComplete  func(string)
	onFailure   func([]byte, []byte)
	responseID  string
	once        sync.Once
}

func (b *openAIHTTPWSBody) finish(clean bool, responseID string) {
	b.once.Do(func() {
		if clean {
			b.onComplete(responseID)
		} else {
			b.lease.MarkBroken()
		}
		b.lease.Release()
	})
}

func (b *openAIHTTPWSBody) Close() error {
	b.finish(false, "")
	return nil
}

func (b *openAIHTTPWSBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 {
		if b.terminal {
			return 0, io.EOF
		}
		message := b.first
		b.first = nil
		if message == nil {
			var err error
			message, err = b.lease.ReadMessageWithContextTimeout(b.ctx, b.readTimeout)
			if err != nil {
				b.finish(false, "")
				return 0, smartWSUncertainError(err)
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, message); err != nil {
			b.finish(false, "")
			return 0, fmt.Errorf("invalid upstream WebSocket event: %w", err)
		}
		eventType := gjson.GetBytes(message, "type").String()
		if id := gjson.GetBytes(message, "response.id").String(); id != "" {
			b.responseID = id
		}
		if eventType == "" {
			b.finish(false, "")
			return 0, errors.New("missing upstream WebSocket event type")
		}
		terminal := normalizeOpenAIWSTerminalEvent(eventType)
		b.observation.event(message)
		if eventType == "error" || terminal == "response.failed" {
			tail := readSmartWSFailedTail(b.ctx, b.lease, message, b.responseID)
			if len(tail) > 0 {
				b.observation.event(tail)
			}
			if b.onFailure != nil {
				b.onFailure(message, tail)
			}
		}
		if terminal != "" || eventType == "error" {
			if b.dispatch != nil {
				b.dispatch.state.Store(smartWSTerminal)
			}
			b.terminal = true
			clean := terminal == "response.completed" || terminal == "response.done" || terminal == "response.incomplete"
			status := gjson.GetBytes(message, "response.status").String()
			clean = clean && status != "failed" && status != "cancelled" && status != "canceled"
			b.finish(clean, strings.TrimSpace(gjson.GetBytes(message, "response.id").String()))
		}
		b.pending = append([]byte("data: "), compact.Bytes()...)
		b.pending = append(b.pending, '\n', '\n')
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
