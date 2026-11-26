package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type riskProbeContextKey struct{}
type riskProbeCall struct {
	config          AccountRiskConfig
	credentialOwner *Account
	result          AccountRiskProbeResult
}

func riskProbeForContext(ctx context.Context) *riskProbeCall {
	v, _ := ctx.Value(riskProbeContextKey{}).(*riskProbeCall)
	return v
}

// Uses the existing account-test authentication, endpoint, proxy and TLS path.
// Upstream I/O only collects evidence. The automatic runner owns scheduling
// changes after reservation validation. Probe usage belongs to the operator.
func (s *AccountTestService) RunRiskProbe(ctx context.Context, a *Account, c AccountRiskConfig) (AccountRiskProbeResult, error) {
	call := &riskProbeCall{config: c}
	if a == nil || a.Platform != PlatformOpenAI || a.IsExcelOAuth() || a.IsExcelBPSEnabledForModel(c.Model) || isOpenAIImageModel(a.GetMappedModel(c.Model)) {
		return call.result, errors.New("unsupported risk probe account/model")
	}
	ctx = context.WithValue(ctx, riskProbeContextKey{}, call)
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = (&http.Request{}).WithContext(ctx)
	startedAt := time.Now().UTC()
	err := s.testOpenAIAccountConnection(gc, a, c.Model, c.ProbePrompt, AccountTestModeDefault)
	// Never retain or log upstream bodies/error messages here.
	if err != nil {
		return call.result, errors.New("risk probe did not complete")
	}
	if call.result.Completed && call.result.UpstreamStatus == http.StatusOK {
		s.openaiGatewayService.reporter401().recovered(ctx, call.credentialOwner, startedAt)
	}
	return call.result, nil
}

func (s *AccountTestService) finishRiskProbeResponse(ctx context.Context, sourceID int64, call *riskProbeCall, resp *http.Response, chat bool) error {
	call.result.UpstreamStatus = resp.StatusCode
	if resp.StatusCode == http.StatusUnauthorized && call.credentialOwner != nil {
		receivedAt := time.Now().UTC()
		target := s.openaiGatewayService.reporter401().targetAt401(ctx, sourceID, call.credentialOwner.ID)
		if call.config.Probe401Enabled {
			call.result.auth401 = readRiskProbe401(call.credentialOwner, call.config.Model, resp.Body)
		}
		if call.result.auth401 == nil {
			call.result.auth401 = &riskProbe401Evidence{ownerID: call.credentialOwner.ID, ownerName: riskNotificationName(call.credentialOwner.Name)}
		}
		evidence := call.result.auth401
		evidence.receivedAt = receivedAt
		evidence.reclaimAt, _ = call.credentialOwner.supplierReclaimAt()
		evidence.alertTarget = target
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("risk probe upstream rejected request")
	}
	result, err := parseRiskProbeResponse(resp.Body, chat, strings.Contains(resp.Header.Get("Content-Type"), "application/json"))
	call.result = result
	if result.UpstreamStatus == http.StatusUnauthorized && call.credentialOwner != nil {
		call.result.auth401 = &riskProbe401Evidence{ownerID: call.credentialOwner.ID, ownerName: riskNotificationName(call.credentialOwner.Name), receivedAt: time.Now().UTC()}
		call.result.auth401.captureAlertTarget(ctx, s.openaiGatewayService.reporter401(), sourceID)
	} else {
		call.result.UpstreamStatus = resp.StatusCode
	}
	return err
}

func parseRiskProbeResponse(body io.Reader, chat, jsonBody bool) (AccountRiskProbeResult, error) {
	var result AccountRiskProbeResult
	limited := &io.LimitedReader{R: body, N: 4 << 20}
	var text strings.Builder
	consume := func(raw []byte) (bool, error) {
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Error    json.RawMessage `json:"error"`
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return false, errors.New("invalid risk probe frame")
		}
		if (len(event.Error) > 0 && string(event.Error) != "null") || event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" {
			if explicitStream401(raw) {
				result.UpstreamStatus = http.StatusUnauthorized
			}
			return false, errors.New("risk probe terminal error")
		}
		if chat {
			for _, c := range event.Choices {
				_, _ = text.WriteString(c.Delta.Content)
				_, _ = text.WriteString(c.Message.Content)
				if c.Finish != nil {
					if *c.Finish != "stop" {
						return false, errors.New("risk probe unfinished chat")
					}
					result.Completed = true
				}
			}
			result.InputTokens = event.Usage.Input
			result.OutputTokens = event.Usage.Output
			if result.Completed {
				result.Text = text.String()
				return true, nil
			}
			return false, nil
		}
		if event.Type == "response.output_text.delta" {
			_, _ = text.WriteString(event.Delta)
		}
		if event.Type == "response.completed" {
			if event.Response.Status != "completed" {
				return false, errors.New("risk probe missing completed status")
			}
			var terminal strings.Builder
			for _, item := range event.Response.Output {
				if item.Type == "message" {
					for _, p := range item.Content {
						if p.Type == "output_text" {
							_, _ = terminal.WriteString(p.Text)
						}
					}
				}
			}
			result = AccountRiskProbeResult{Completed: true, Text: terminal.String(), InputTokens: event.Response.Usage.Input, OutputTokens: event.Response.Usage.Output}
			if len(event.Response.Output) == 0 {
				result.Text = text.String()
			}
			return true, nil
		}
		return false, nil
	}
	if jsonBody {
		raw, err := io.ReadAll(limited)
		if err != nil || limited.N == 0 {
			return result, errors.New("risk probe body incomplete or too large")
		}
		if !chat {
			raw = append(append([]byte(`{"type":"response.completed","response":`), raw...), '}')
		}
		done, err := consume(raw)
		if err == nil && !done {
			err = errors.New("risk probe missing terminal")
		}
		return result, err
	}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data []string
	flush := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return false, errors.New("risk probe missing terminal")
		}
		return consume([]byte(raw))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := flush()
			if done || err != nil {
				return result, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil || limited.N == 0 {
		return result, errors.New("risk probe stream incomplete or too large")
	}
	done, err := flush()
	if err == nil && !done {
		err = errors.New("risk probe stream ended without terminal")
	}
	return result, err
}
