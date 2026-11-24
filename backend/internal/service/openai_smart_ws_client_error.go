package service

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Keep the protocol envelope (including terminal usage) on the client-facing
// copy. Retry, scheduling and billing still classify the original response.
func smartWSClientError(c *gin.Context, account *Account, status int, body []byte) (int, []byte) {
	publicStatus, _, custom, rule, matched := applyErrorPassthroughRuleWithRule(c, PlatformOpenAI, status, body, status, "upstream_error", "Upstream request failed")
	root := gjson.ParseBytes(body)
	if !root.IsObject() && !matched {
		return publicStatus, []byte(sanitizeOpenAIPassthroughClientErrorText(account, truncateString(string(body), 4096)))
	}
	copyFields := func(obj gjson.Result, keys ...string) gin.H {
		out := gin.H{}
		for _, key := range keys {
			v := obj.Get(key)
			switch v.Type {
			case gjson.Number:
				out[key] = json.Number(v.Raw)
			case gjson.String:
				if text := openAIPassthroughClientErrorField(account, v); text != "" {
					out[key] = text
				}
			}
		}
		return out
	}
	errorObject := func(obj gjson.Result) gin.H {
		if matched && !rule.PassthroughBody {
			if rule.CustomMessage == nil {
				custom = sanitizeOpenAIPassthroughClientErrorText(account, custom)
			}
			if custom == "" {
				custom = "Upstream request failed"
			}
			return gin.H{"type": "upstream_error", "message": custom}
		}
		out := copyFields(obj, "code", "type", "param", "status", "status_code", "retry_after", "retry_after_ms", "resets_at", "resets_in_seconds")
		if msg := openAIWSLogReasonValue(obj.Get("message")); msg.Type == gjson.String {
			out["message"] = sanitizeOpenAIPassthroughClientErrorText(account, truncateString(msg.Str, 4096))
		}
		if code, _ := out["code"].(string); strings.EqualFold(code, "server_is_overloaded") || strings.EqualFold(code, "slow_down") {
			out["code"] = openAICapacityShedRetryableClientCode
		}
		if matched && publicStatus != status {
			if _, ok := out["status"]; ok {
				out["status"] = publicStatus
			}
			if _, ok := out["status_code"]; ok {
				out["status_code"] = publicStatus
			}
		}
		return out
	}
	envelope := func(obj gjson.Result, keys ...string) gin.H {
		out := copyFields(obj, keys...)
		if u := obj.Get("usage"); u.IsObject() {
			usage := copyFields(u, "input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")
			for _, k := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
				if d := u.Get(k); d.IsObject() {
					usage[k] = copyFields(d, "cached_tokens", "reasoning_tokens", "image_tokens", "audio_tokens", "text_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens")
				}
			}
			out["usage"] = usage
		}
		if e := obj.Get("error"); e.IsObject() {
			out["error"] = errorObject(e)
		}
		return out
	}
	out := envelope(root, "type", "id", "object", "event_id", "response_id", "sequence_number", "created_at", "status", "status_code", "retry_after", "retry_after_ms", "model", "service_tier")
	// Some relays use a flat error event; its code and message are part of
	// the wire protocol too. Do not copy arbitrary diagnostic fields.
	if root.Get("type").Str == "error" && !root.Get("error").Exists() && (root.Get("code").Exists() || root.Get("message").Exists()) {
		flat := errorObject(root)
		for _, k := range []string{"code", "message", "param"} {
			if v, ok := flat[k]; ok {
				out[k] = v
			}
		}
		if matched && !rule.PassthroughBody {
			out["error"] = flat
		}
	}
	response := root.Get("response")
	if response.IsObject() {
		r := envelope(response, "id", "object", "status", "created_at", "model", "service_tier")
		if response.Get("output").IsArray() {
			r["output"] = []any{}
		}
		out["response"] = r
	}
	hasText := openAIWSLogReasonValue(response.Get("error.message")).Type == gjson.String || openAIWSLogReasonValue(root.Get("error.message")).Type == gjson.String
	if _, flat := out["message"]; !flat && !hasText && (!root.Get("error.code").Exists() && !response.Get("error.code").Exists()) {
		msg := openAIWSLogErrorMessage(body)
		if matched && !rule.PassthroughBody {
			out["error"] = errorObject(gjson.Result{})
		} else {
			key := "error"
			target := out
			if nested, ok := out["response"].(gin.H); ok && root.Get("type").Str == "response.failed" {
				target = nested
			}
			e, ok := target[key].(gin.H)
			if !ok {
				e = gin.H{}
			}
			e["message"] = sanitizeOpenAIPassthroughClientErrorText(account, msg)
			target[key] = e
		}
	}
	if publicStatus != status {
		if _, ok := out["status"]; ok {
			out["status"] = publicStatus
		}
		if _, ok := out["status_code"]; ok {
			out["status_code"] = publicStatus
		}
	}
	result, err := json.Marshal(out)
	if err != nil {
		return publicStatus, []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)
	}
	return publicStatus, result
}

func smartWSClientFrame(c *gin.Context, a *Account, payload []byte) []byte {
	if a == nil || a.Platform != PlatformOpenAI {
		return payload
	}
	typ := gjson.GetBytes(payload, "type").Str
	if typ != "error" && typ != "response.failed" {
		return payload
	}
	_, out := smartWSClientError(c, a, openAIHTTPWSEventErrorStatus(payload), payload)
	return out
}

// PreservedClientError renders an exhausted retry using its original account
// for redaction. The retry owner retains the unchanged response for matching.
func (e *UpstreamFailoverError) PreservedClientError(c *gin.Context) (int, string, string, string) {
	status, body := smartWSClientError(c, e.clientErrorAccount, e.StatusCode, e.ResponseBody)
	typ, code, message := OpenAIUpstreamErrorFields(status, body)
	if flat := gjson.GetBytes(body, "code"); flat.Type == gjson.String && !gjson.GetBytes(body, "error").Exists() {
		code = flat.Str
	}
	return status, typ, code, message
}
