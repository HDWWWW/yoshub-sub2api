package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
				Background         bool            `json:"background"`
				Reasoning          json.RawMessage `json:"reasoning,omitempty"`
				ServiceTier        json.RawMessage `json:"service_tier,omitempty"`
			}
			err = json.NewDecoder(copy).Decode(&continuation)
			_ = copy.Close()
			if err != nil {
				return nil, err
			}
			observationRequest, err = json.Marshal(continuation)
			if err != nil {
				return nil, err
			}
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
		if response == nil {
			return nil, smartWSUncertainError(errors.New("empty upstream HTTP response"))
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

	if response, handled, poolErr := s.tryIndependentSmartWS(c, req, raw, encoded, proxyURL, account, observation, dispatch); handled {
		return response, poolErr
	}
	return fallback()
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
