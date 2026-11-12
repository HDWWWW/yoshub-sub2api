package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/smartws"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// SmartWS has a separate physical pool from native WS ingress. Account admission,
// proxy selection, request preparation and usage remain owned by the gateway.
func (s *OpenAIGatewayService) getIndependentSmartWSPool() *smartws.Pool {
	s.openaiSmartPoolOnce.Do(func() {
		if s.openaiSmartPool == nil {
			s.openaiSmartPool = smartws.NewPool()
		}
	})
	return s.openaiSmartPool
}

func (s *OpenAIGatewayService) tryIndependentSmartWS(c *gin.Context, req *http.Request, raw, encoded []byte, proxyURL string, account *Account, observation *smartWSObservation, dispatch *smartWSDispatch) (*http.Response, bool, error) {
	// The caller has already checked group/account transport policy and built the
	// validated destination. Scope and proxy remain part of exact connection reuse.
	scope := fmt.Sprintf("group:%d/account:%d/key:%d/scope:%s", getOpenAIGroupIDFromContext(c), account.ID, getAPIKeyIDFromContext(c), c.GetString(openAIHTTPWSExecutionScopeKey))
	copyReq := req.Clone(req.Context())
	copyReq.Body = io.NopCloser(bytes.NewReader(raw))
	copyReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	copyReq.ContentLength = int64(len(raw))
	headers, err := s.refreshOpenAIAgentIdentityHeaders(req.Context(), account, copyReq.Header)
	if err != nil {
		return nil, true, err
	}
	copyReq.Header = headers
	var frameLimit int64
	if s.cfg != nil {
		frameLimit = s.cfg.Gateway.OpenAIWS.MaxUpstreamFrameBytes
	}
	var response *http.Response
	response, handled, err := s.getIndependentSmartWSPool().TryWithOptions(copyReq, proxyURL, scope, smartws.Options{
		OnHandshake: func(status int, h http.Header, body []byte, dialErr error, elapsed time.Duration) {
			observation.m.handshakes.Add(1)
			if dialErr != nil {
				observation.m.recordHandshakeFailure(status)
				dialErr = &openAIWSHandshakeError{Body: body, Err: dialErr}
			}
			s.getOpenAIWSConnPool().observeSmartWSHandshake(req.Context(), openAIWSAcquireRequest{Account: account, WSURL: copyReq.URL.String(), Headers: copyReq.Header, ProxyURL: proxyURL, IsolationKey: "http:" + scope}, dialErr == nil, status, h, dialErr, elapsed)
		},
		AllowTarget:      true,
		WireMessage:      encoded,
		ReadTimeout:      s.openAIWSReadTimeout(),
		MaxFrameBytes:    frameLimit,
		RequireWebSearch: account.IsOpenAIOAuthLike() && openAIWSWebSearchDeclared(encoded),
		OnFailure: func(first, tail []byte) []byte {
			dispatch.observe(c, tail)
			first = dispatch.failurePayload(first)
			rememberSmartWSHTTPFailure(c, response.Header, raw, first, tail, observation.start)
			return first
		},
		OnDispatch: func() { observation.dispatched = true; dispatch.state.Store(smartWSMayBeSent) },
		OnEvent: func(payload []byte) {
			dispatch.observe(c, payload)
			observation.event(payload)
			typ := gjson.GetBytes(payload, "type").String()
			if response != nil {
				rememberSmartWSHTTPUsage(c, response.Header, raw, payload, observation.start, true)

			}
			if isOpenAIWSTerminalEvent(typ) || typ == "error" {
				dispatch.state.Store(smartWSTerminal)
			} else {
				dispatch.state.Store(smartWSReceived)
			}
		},
	})
	if err != nil {
		return response, handled, smartWSUncertainError(err)
	}
	if !handled {
		return nil, false, nil
	}
	if response != nil && response.Header.Get("X-Sub2-Upstream-Transport") == "websocket" {
		observation.reused = response.Header.Get("X-Sub2-WS-Reused") == "true"
		response.Header.Del("X-Sub2-WS-Reused")
		response.Header.Del("X-Sub2-Upstream-Transport")
		response.Request = copyReq.WithContext(context.WithValue(copyReq.Context(), openAIHTTPWSTransportKey{}, true))
		c.Set(OpsOpenAIWSConnReusedKey, observation.reused)
		c.Set("openai_ws_transport_decision", string(OpenAIUpstreamTransportResponsesWebsocketV2))
		c.Set("openai_ws_transport_reason", "independent_smart_ws")
		// Inspect one complete event before opening downstream SSE. An immediate
		// rejection keeps its HTTP status and never triggers a happy notification.
		source := response.Body
		reader := bufio.NewReader(source)
		var first bytes.Buffer
		var payload []byte
		for first.Len() <= 16<<20 {
			line, readErr := reader.ReadString('\n')
			_, _ = first.WriteString(line)
			if strings.HasPrefix(line, "data: ") {
				payload = append(payload, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data: ")))...)
			}
			if readErr != nil {
				_ = source.Close()
				return nil, true, smartWSUncertainError(readErr)
			}
			if line == "\n" {
				break
			}
		}
		if first.Len() > 16<<20 {
			_ = source.Close()
			return nil, true, smartWSUncertainError(errors.New("upstream event exceeds limit"))
		}
		typ := gjson.GetBytes(payload, "type").String()
		if typ == "error" || typ == "response.failed" {
			_ = source.Close()
			return openAIHTTPWSErrorResponse(copyReq, openAIHTTPWSEventErrorStatus(payload), response.Header, payload), true, nil
		}
		response.Body = &smartWSPrefixedBody{Reader: io.MultiReader(bytes.NewReader(first.Bytes()), reader), Closer: source}
	} else if response != nil && response.StatusCode >= 400 {
		// This is an explicit handshake rejection, before generation dispatch.
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, true, readErr
		}
		response = smartWSHandshakeErrorResponse(copyReq, &openAIWSDialError{StatusCode: response.StatusCode, ResponseHeaders: response.Header, ResponseBody: payload})
	}
	return response, true, nil
}

// A pool has its own counters; the surrounding gateway retains actual-content
// latency samples and native metrics instead of mixing two capacity budgets.
func (s *OpenAIGatewayService) independentSmartWSStats() map[string]any {
	if s == nil {
		return nil
	}
	return s.getIndependentSmartWSPool().Stats()
}

type smartWSPrefixedBody struct {
	io.Reader
	io.Closer
}
