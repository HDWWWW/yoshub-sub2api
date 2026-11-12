package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// A confirmed rejection is distinct from both an unsent request and an
// uncertain dispatch. Only this proof can reopen the gateway's retry policy.
const smartWSRejected int32 = -1

func (d *smartWSDispatch) observe(c *gin.Context, payload []byte) {
	if d == nil || len(payload) == 0 {
		return
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
	typ := gjson.GetBytes(payload, "type").String()
	if (typ == "error" || typ == "response.failed") && !smartWSIsExplicitRateLimit(payload) {
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

// Call only before delivering the error, and only for the initial native WS
// turn (HTTP requests already carry full input). Retry count, Retry-After and
// cross-account recovery deadlines remain owned by the existing handler.
func (s *OpenAIGatewayService) smartWSRateLimitRejection(c *gin.Context, account *Account, status int, headers http.Header, payload []byte, handshake bool, model string) *UpstreamFailoverError {
	if c == nil || account == nil || account.Platform != PlatformOpenAI || status != http.StatusTooManyRequests ||
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
		if (gjson.GetBytes(payload, "error").Exists() || gjson.GetBytes(payload, "response.error").Exists()) && !smartWSIsExplicitRateLimit(payload) {
			return nil
		}
	} else if d == nil || d.accepted.Load() || !smartWSIsExplicitRateLimit(payload) {
		return nil
	}
	if d != nil {
		d.state.Store(smartWSRejected)
	}
	code, typ, message := parseOpenAIWSErrorEventFields(payload)
	if handshake && code == "" && typ == "" {
		code, typ = "rate_limit_exceeded", "rate_limit_error"
	}
	s.persistOpenAIWSRateLimitSignal(c.Request.Context(), account, headers, payload, code, typ, message, model)
	return s.newOpenAIWSRateLimitFailoverError(account, headers, payload, message)
}
