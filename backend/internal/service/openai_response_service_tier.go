package service

import (
	"bytes"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponseServiceTierContextKey = "openai_response_outbound_service_tier"

func setOpenAIResponseServiceTier(c *gin.Context, tier *string) {
	if c != nil {
		c.Set(openAIResponseServiceTierContextKey, optionalStringValue(tier))
	}
}

func rewriteOpenAIResponseServiceTierFromContext(c *gin.Context, account *Account, payload []byte) []byte {
	if c == nil {
		return payload
	}
	tier := c.GetString(openAIResponseServiceTierContextKey)
	return rewriteOpenAIResponseServiceTier(payload, account, &tier)
}

// Rewrite only Codex's non-authoritative default echo, using the same resolver
// as billing. Call on the client copy after observing the raw upstream payload.
func rewriteOpenAIResponseServiceTier(payload []byte, account *Account, requested *string) []byte {
	if account == nil || !account.IsOpenAIOAuthLike() || requested == nil {
		return payload
	}
	resolution := ResolveOpenAIServiceTierBilling(account, *requested, "default")
	switch resolution.Billing {
	case OpenAIFastTierPriority, OpenAIFastTierUltrafast:
	default:
		return payload
	}
	if !bytes.Contains(payload, []byte(`"service_tier"`)) {
		return payload
	}
	isResponseObject := gjson.GetBytes(payload, "object").String() == "response" ||
		(gjson.GetBytes(payload, "id").Type == gjson.String && gjson.GetBytes(payload, "model").Type == gjson.String &&
			!gjson.GetBytes(payload, "type").Exists())
	if !gjson.GetBytes(payload, "response").IsObject() && !isResponseObject {
		return payload
	}
	for _, path := range []string{"response.service_tier", "service_tier"} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String || !codexOAuthResponseTierIsNonAuthoritative(value.String()) {
			continue
		}
		if !gjson.ValidBytes(payload) {
			return payload
		}
		if rewritten, err := sjson.SetBytes(payload, path, resolution.Billing); err == nil {
			payload = rewritten
		}
	}
	return payload
}
