package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAICyberAccessDeniedMessage = "The requested Cyber access program is not authorized for this model."

var errOpenAICyberAccessDenied = errors.New("cyber access denied for requested model")

// This specific program/model entitlement rejection describes the request,
// not the credential's health. Do not rotate accounts or penalize their other
// traffic. Match only provider error fields, never echoed request content.
func isOpenAICyberAccessDenied(statusCode int, body []byte) bool {
	if statusCode != http.StatusForbidden || !gjson.ValidBytes(body) || openAIStreamCredentialAuthFailure(body) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "detail.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code != "" && code != "permission_denied" && code != "permission_error" {
			return false // Explicit credential/account failure codes retain their policy.
		}
	}
	for _, path := range []string{"detail", "error.message", "response.error.message", "detail.message", "message"} {
		message := gjson.GetBytes(body, path)
		if message.Type == gjson.String && strings.EqualFold(strings.TrimSpace(message.String()), openAICyberAccessDeniedMessage) {
			return true
		}
	}
	return false
}

func writeOpenAICyberAccessDenied(c *gin.Context, account *Account, resp *http.Response, body []byte, writeError compatErrorWriter) error {
	setOpsUpstreamError(c, resp.StatusCode, openAICyberAccessDeniedMessage, truncateString(string(body), 2048))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "http_error", Message: openAICyberAccessDeniedMessage,
	})
	MarkResponseCommitted(c)
	// A deterministic permission rejection must not advertise a retry delay.
	writeOpenAIPassthroughErrorHeaders(c.Writer.Header(), nil)
	if writeError != nil {
		writeError(c, http.StatusForbidden, "permission_error", openAICyberAccessDeniedMessage)
	} else {
		payload, _ := json.Marshal(gin.H{"error": gin.H{
			"type": "permission_error", "message": openAICyberAccessDeniedMessage,
		}})
		if !writeOpenAICompactSSEBridge(c, http.StatusForbidden, payload) {
			c.Data(http.StatusForbidden, "application/json; charset=utf-8", payload)
		}
	}
	// Do not mark this as cyber_policy: a missing program entitlement is not a
	// content moderation finding and must not ban the user or session.
	return errOpenAICyberAccessDenied
}
