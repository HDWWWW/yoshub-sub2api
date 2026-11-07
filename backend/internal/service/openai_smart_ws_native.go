package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// HTTP handshakes have no WS envelope. Keep the upstream error object intact
// and add only the envelope and the safe retry hint needed by a WS client.
func smartWSNativeErrorPayload(status int, body []byte, headers http.Header) []byte {
	if typ := gjson.GetBytes(body, "type").String(); typ == "error" || typ == "response.failed" {
		return body
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		body, _ = json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "message": string(body)}})
	}
	body, _ = sjson.SetBytes(body, "type", "error")
	body, _ = sjson.SetBytes(body, "status", status)
	if after := headers.Get("Retry-After"); after != "" {
		body, _ = sjson.SetBytes(body, "retry_after", after)
	}
	return body
}

func writeSmartWSNativeFailure(c *gin.Context, status int, body []byte, headers http.Header, write func([]byte) error) error {
	payload := smartWSNativeErrorPayload(status, body, headers)
	if err := write(payload); err != nil {
		return err
	}
	c.Set(smartWSFailureDeliveredKey, true)
	markOpenAIWSClientVisibleFailure(c, gjson.GetBytes(payload, "type").String(), payload)
	return smartWSStopFailure(status, body, headers)
}

func writeSmartWSNativeUncertain(c *gin.Context, conn *coderws.Conn, err error) error {
	if err == nil || c == nil || conn == nil || !smartWSOutcomeUncertain(c) || SmartWSFailureDelivered(c) {
		return err
	}
	payload, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{
		"type": "upstream_error", "code": smartWSUncertainCode, "message": smartWSUncertainMessage,
	}})
	// An internal relay cancellation must not cancel this final notification.
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn.Write(writeCtx, coderws.MessageText, payload) == nil {
		c.Set(smartWSFailureDeliveredKey, true)
		markOpenAIWSClientVisibleFailure(c, "error", payload)
	}
	var closeErr *OpenAIWSClientCloseError
	if errors.As(err, &closeErr) {
		return err
	}
	return NewOpenAIWSClientCloseError(coderws.StatusInternalError, smartWSUncertainCode+"; request was not replayed", err)
}

// Explicit handshake responses are terminal upstream errors, even before
// dispatch. A transport-level 403 is not evidence of a revoked credential.
func (s *OpenAIGatewayService) smartWSNativeHandshakeFailure(ctx context.Context, c *gin.Context, account *Account, model string, err error, write func([]byte) error) error {
	var dialErr *openAIWSDialError
	if account.Platform != PlatformOpenAI || !errors.As(err, &dialErr) || dialErr.StatusCode < 400 {
		return nil
	}
	body := s.redactAgentIdentitySensitiveBody(ctx, account, dialErr.ResponseBody)
	classification := classifySmartWSHandshake(dialErr.StatusCode, body)
	var usage OpenAIUsage
	parseOpenAIWSResponseUsageFromCompletedEvent(body, &usage)
	if !markOpenAICyberPolicyEvent(c, body, dialErr.StatusCode, &usage) && classification.accountFailure {
		_ = s.handleOpenAIAccountUpstreamError(ctx, account, dialErr.StatusCode, dialErr.ResponseHeaders, body, model)
	}
	message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
	setOpsUpstreamError(c, dialErr.StatusCode, message, "")
	event := OpsUpstreamErrorEvent{AccountID: account.ID, Platform: account.Platform, UpstreamStatusCode: dialErr.StatusCode,
		Kind: "ws_upstream_error", Stage: "ws_handshake", Scope: "upstream_transport", Reason: classification.category, Message: message}
	if classification.accountFailure {
		event.Scope = "account"
	}
	event.ProxyID, event.ProxyName = opsUpstreamWSProxyAttribution(account)
	appendOpsUpstreamError(c, event)
	failure := writeSmartWSNativeFailure(c, dialErr.StatusCode, body, dialErr.ResponseHeaders, write)
	var stop *UpstreamFailoverError
	if errors.As(failure, &stop) {
		classification.annotate(stop)
	}
	return failure
}
