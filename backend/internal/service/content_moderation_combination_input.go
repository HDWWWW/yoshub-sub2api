package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

type combinationMessage struct {
	text, source string
	current      bool
}

// Only texts actually present in this request are inspectable. Do not join
// separate turns, scan encrypted reasoning, or treat image/base64 data as text.
func extractCombinationMessages(protocol string, body []byte) []combinationMessage {
	if !gjson.ValidBytes(body) {
		return nil
	}
	out := []combinationMessage{}
	add := func(value gjson.Result, source string) {
		role := strings.ToLower(value.Get("role").String())
		if role == "system" || role == "developer" {
			return
		}
		var parts []string
		collectCombinationText(value, &parts)
		if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
			out = append(out, combinationMessage{text: text, source: source})
		}
	}
	fields := []string{"input", "messages", "contents"}
	switch protocol {
	case ContentModerationProtocolOpenAIResponses:
		fields = []string{"input"}
	case ContentModerationProtocolOpenAIChat, ContentModerationProtocolAnthropicMessages:
		fields = []string{"messages"}
	case ContentModerationProtocolGemini:
		fields = []string{"contents"}
	case ContentModerationProtocolOpenAIImages:
		fields = []string{"prompt"}
	case ContentModerationProtocolTypeSafeSystemOne:
		fields = nil
	}
	for _, field := range fields {
		value := gjson.GetBytes(body, field)
		if value.IsArray() {
			value.ForEach(func(index, item gjson.Result) bool {
				add(item, fmt.Sprintf("%s[%d]", field, index.Int()))
				return true
			})
		} else {
			add(value, field)
		}
	}
	if current := extractContentModerationKeywordText(protocol, body); current != "" {
		out = append(out, combinationMessage{text: current, source: "current_user", current: true})
	}
	return out
}

func collectCombinationText(value gjson.Result, parts *[]string) {
	if value.Type == gjson.String {
		*parts = append(*parts, value.String())
		return
	}
	if value.IsArray() {
		value.ForEach(func(_, item gjson.Result) bool { collectCombinationText(item, parts); return true })
		return
	}
	if !value.IsObject() {
		return
	}
	switch strings.ToLower(value.Get("type").String()) {
	case "reasoning", "image", "image_url", "input_image", "input_audio", "audio", "file", "input_file":
		return
	case "function_call", "tool_use":
		for _, field := range []string{"arguments", "input"} {
			collectCombinationData(value.Get(field), parts)
		}
	case "custom_tool_call":
		collectCombinationData(value.Get("input"), parts)
	}
	for _, field := range []string{"text", "content", "parts", "output"} {
		collectCombinationText(value.Get(field), parts)
	}
	for _, field := range []string{"functionCall.args", "function_call.args", "functionResponse.response", "function_response.response"} {
		collectCombinationData(value.Get(field), parts)
	}
	if calls := value.Get("tool_calls"); calls.IsArray() {
		calls.ForEach(func(_, call gjson.Result) bool {
			collectCombinationData(call.Get("function.arguments"), parts)
			return true
		})
	}
}

func collectCombinationData(value gjson.Result, parts *[]string) {
	if value.Type == gjson.String {
		*parts = append(*parts, value.String())
	} else if value.IsObject() || value.IsArray() {
		value.ForEach(func(_, child gjson.Result) bool { collectCombinationData(child, parts); return true })
	}
}
