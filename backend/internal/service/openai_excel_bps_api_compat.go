package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *OpenAIGatewayService) shouldUseExcelBPSCompat(ctx context.Context, a *Account, body []byte, defaultModel string) bool {
	model := resolveOpenAIForwardModel(a, gjson.GetBytes(body, "model").String(), defaultModel)
	return a.IsExcelOAuth() || (a.isExcelBPSUpstreamModelEnabled(model) && s.excelBPSGloballyEnabled(ctx))
}

// Convert before any Codex-specific normalization or continuation trimming.
// Only the response format changes; BPS owns cancellation, usage and retry rules.
func (s *OpenAIGatewayService) forwardExcelBPSCompat(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultModel string, anthropic bool) (*OpenAIForwardResult, error) {
	start := time.Now()
	model := gjson.GetBytes(body, "model").String()
	stream := gjson.GetBytes(body, "stream").Bool()
	billingModel := resolveOpenAIForwardModel(account, model, defaultModel)
	if anthropic {
		billingModel = resolveOpenAIForwardModel(account, NormalizeOpenAICompatRequestedModel(model), defaultModel)
	}
	w := &excelBPSCompatWriter{ResponseWriter: c.Writer, anthropic: anthropic, model: model}
	w.chat = apicompat.NewResponsesEventToChatState()
	w.chat.Model = model
	w.chat.IncludeUsage = gjson.GetBytes(body, "stream_options.include_usage").Bool()
	w.messages = apicompat.NewResponsesEventToAnthropicState()
	w.messages.Model = model
	c.Writer = w
	defer func() { c.Writer = w.ResponseWriter }()

	// The mapping has already been resolved, including the group's default. A
	// shallow account copy avoids applying chained mappings for a second time.
	a := *account
	a.Credentials = maps.Clone(account.Credentials)
	delete(a.Credentials, "model_mapping")
	a.modelMappingCache = nil
	if err := s.validateExcelOAuthRoute(ctx, &a, billingModel); err != nil {
		return nil, writeExcelOAuthRouteError(c)
	}
	var request *apicompat.ResponsesRequest
	var err error
	if anthropic {
		var req apicompat.AnthropicRequest
		if err = json.Unmarshal(body, &req); err == nil {
			applyOpenAICompatModelNormalization(&req)
			req.Model = billingModel
			request, err = anthropicToResponses(s.cfg, &req)
			if err == nil && request.Reasoning != nil {
				request.Reasoning.Effort = openAICompatAnthropicReasoningEffort(&req, billingModel, request.Reasoning.Effort)
			}
		}
	} else if !gjson.GetBytes(body, "messages").Exists() && gjson.GetBytes(body, "input").Exists() {
		err = json.Unmarshal(body, &request)
	} else {
		var req apicompat.ChatCompletionsRequest
		if err = json.Unmarshal(body, &req); err == nil {
			req.Model = billingModel
			request, err = chatCompletionsToResponses(s.cfg, &req)
		}
	}
	if err != nil || request == nil {
		if err == nil {
			err = fmt.Errorf("empty request")
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
		return nil, err
	}
	request.Model, request.Stream = billingModel, stream
	body, err = json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if body, _, err = ApplyOpenAIReasoningEffortPolicyFromContext(ctx, body); err != nil {
		return nil, err
	}
	upstreamCtx, release := openAIAccountUpstreamContext(ctx, account)
	defer release()
	result, err := s.forwardExcelBPS(upstreamCtx, c, &a, body, start)
	if result != nil {
		result.Model, result.BillingModel, result.Stream = model, billingModel, stream
	}
	return result, err
}

// BPS emits complete JSON bodies or line-oriented SSE. Convert synchronously
// at the writer boundary to retain backpressure and the request's cancellation
// policy, without buffering a whole stream or sharing gin.Context with a worker.
type excelBPSCompatWriter struct {
	gin.ResponseWriter
	anthropic bool
	model     string
	pending   []byte
	terminal  bool
	chat      *apicompat.ResponsesEventToChatState
	messages  *apicompat.ResponsesEventToAnthropicState
}

func (w *excelBPSCompatWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *excelBPSCompatWriter) Write(p []byte) (int, error) {
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		out := p
		if gjson.GetBytes(p, "error").Exists() {
			if w.anthropic {
				out, _ = sjson.SetBytes(p, "type", "error")
			}
		} else {
			var response apicompat.ResponsesResponse
			if err := json.Unmarshal(p, &response); err != nil {
				return 0, err
			}
			var err error
			if w.anthropic {
				out, err = json.Marshal(apicompat.ResponsesToAnthropic(&response, w.model))
			} else {
				out, err = json.Marshal(apicompat.ResponsesToChatCompletions(&response, w.model))
			}
			if err != nil {
				return 0, err
			}
		}
		_, err := w.ResponseWriter.Write(out)
		return len(p), err
	}
	w.pending = append(w.pending, p...)
	if len(w.pending) > 16<<20 {
		return 0, fmt.Errorf("BPS compatibility event exceeds limit")
	}
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			break
		}
		line := strings.TrimSuffix(string(w.pending[:end]), "\r")
		w.pending = w.pending[end+1:]
		if err := w.writeSSELine(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *excelBPSCompatWriter) writeSSELine(line string) error {
	if w.terminal {
		return nil
	}
	if strings.HasPrefix(line, ":") {
		_, err := w.ResponseWriter.WriteString(line + "\n\n")
		return err
	}
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return nil
	}
	var event apicompat.ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.Type == "error" || event.Type == "response.failed" || event.Type == "response.cancelled" {
		w.terminal = true
		message := gjson.Get(payload, "response.error.message").String()
		if message == "" {
			message = gjson.Get(payload, "error.message").String()
		}
		if message == "" {
			message = "Excel BPS response failed"
		}
		code := gjson.Get(payload, "response.error.code").String()
		if code == "" {
			code = "basispoints_stream_failed"
		}
		out := buildChatStreamErrorSSE(code, message)
		if w.anthropic {
			out = buildAnthropicStreamErrorSSE("api_error", message)
		}
		_, err := w.ResponseWriter.WriteString(out)
		return err
	}
	if w.anthropic {
		for _, item := range apicompat.ResponsesEventToAnthropicEvents(&event, w.messages) {
			out, err := apicompat.ResponsesAnthropicEventToSSE(item)
			if err != nil {
				return err
			}
			if _, err = w.ResponseWriter.WriteString(out); err != nil {
				return err
			}
		}
	} else {
		for _, item := range apicompat.ResponsesEventToChatChunks(&event, w.chat) {
			out, err := apicompat.ChatChunkToSSE(item)
			if err != nil {
				return err
			}
			if _, err = w.ResponseWriter.WriteString(out); err != nil {
				return err
			}
		}
	}
	if event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.done" {
		w.terminal = true
		if !w.anthropic {
			_, err := w.ResponseWriter.WriteString("data: [DONE]\n\n")
			return err
		}
	}
	return nil
}
