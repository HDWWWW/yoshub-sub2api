package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

const moderationUserBodyScope = "user_body"

type moderationUserBody struct {
	text, source string
}

// This extractor is ONLY for retained audit evidence. Never feed its filtered
// output back into keyword/rule checks, semantic moderation or the hash cache.
// Tool continuations may end without a user message, so walk the supplied
// history and keep the latest meaningful user text, with its original source.
func extractModerationUserBodies(protocol string, body []byte) []moderationUserBody {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	field, implicitUser := "", false
	switch protocol {
	case ContentModerationProtocolOpenAIResponses:
		field, implicitUser = "input", true
	case ContentModerationProtocolOpenAIChat, ContentModerationProtocolAnthropicMessages:
		field = "messages"
	case ContentModerationProtocolGemini:
		field, implicitUser = "contents", true
	case ContentModerationProtocolOpenAIImages:
		field, implicitUser = "prompt", true
	case ContentModerationProtocolTypeSafeSystemOne:
		// System One's user data is state; question instructions are configuration.
		var parts []string
		collectModerationUserState(root.Get("state"), &parts)
		if text := cleanModerationUserBody(strings.Join(parts, "\n")); text != "" {
			return []moderationUserBody{{text: text, source: "state"}}
		}
		return nil
	default:
		// Official policy events carry the original request but no protocol enum.
		for _, candidate := range []string{"input", "messages", "contents", "prompt"} {
			if root.Get(candidate).Exists() {
				field, implicitUser = candidate, candidate != "messages"
				break
			}
		}
	}
	if field == "" {
		return nil
	}
	var out []moderationUserBody
	add := func(value gjson.Result, source string) {
		role := strings.ToLower(strings.TrimSpace(value.Get("role").String()))
		if role != "user" && !(role == "" && implicitUser) {
			return
		}
		var parts []string
		collectModerationUserText(value, &parts)
		if text := cleanModerationUserBody(strings.Join(parts, "\n")); text != "" {
			out = append(out, moderationUserBody{text: text, source: source})
		}
	}
	value := root.Get(field)
	if value.IsArray() {
		value.ForEach(func(index, item gjson.Result) bool {
			add(item, fmt.Sprintf("%s[%d]", field, index.Int()))
			return true
		})
	} else {
		add(value, field)
	}
	return out
}

func collectModerationUserText(value gjson.Result, parts *[]string) {
	switch {
	case value.Type == gjson.String:
		if text := cleanModerationUserBody(value.String()); text != "" {
			*parts = append(*parts, text)
		}
	case value.IsArray():
		value.ForEach(func(_, child gjson.Result) bool {
			collectModerationUserText(child, parts)
			return true
		})
	case value.IsObject():
		if role := strings.ToLower(strings.TrimSpace(value.Get("role").String())); role != "" && role != "user" {
			return
		}
		// Explicit allowlist: don't recurse into tool results, arguments, files,
		// image URLs/base64, reasoning or a Gemini functionResponse's text.
		switch strings.ToLower(value.Get("type").String()) {
		case "", "message", "text", "input_text":
		default:
			return
		}
		if value.Get("thought").Bool() {
			return
		}
		for _, field := range []string{"text", "content", "parts"} {
			collectModerationUserText(value.Get(field), parts)
		}
	}
}

func collectModerationUserState(value gjson.Result, parts *[]string) {
	if value.Type == gjson.String {
		*parts = append(*parts, value.String())
	} else if value.IsArray() || value.IsObject() {
		value.ForEach(func(key, child gjson.Result) bool {
			if !isModerationSecretField(key.String()) {
				collectModerationUserState(child, parts)
			}
			return true
		})
	}
}

var moderationClientBlocks = []*regexp.Regexp{
	regexp.MustCompile(`(?is)^\s*# AGENTS\.md instructions[^\r\n]*\r?\n+\s*<INSTRUCTIONS>.*?</INSTRUCTIONS>`),
	regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?is)<system_reminder>.*?</system_reminder>`),
	regexp.MustCompile(`(?is)<environment_context>.*?</environment_context>`),
	regexp.MustCompile(`(?is)<environment_details>.*?</environment_details>`),
}

func cleanModerationUserBody(text string) string {
	text = strings.TrimSpace(text)
	// Known Codex compaction envelope, not a new human message.
	if strings.HasPrefix(text, "Another language model started to solve this problem and produced a summary of its thinking process.") {
		return ""
	}
	for _, block := range moderationClientBlocks {
		text = block.ReplaceAllString(text, "\n")
	}
	text = strings.TrimSpace(text)
	// Only unwrap recognized client envelopes; a user's ordinary headings,
	// code, XML and JSON remain text rather than being interpreted as messages.
	for _, prefix := range []string{"# Context from my IDE setup:", "# Files pasted by the user:", "# Files mentioned by the user:"} {
		if strings.HasPrefix(text, prefix) {
			if index := strings.Index(text, "\n## My request:"); index >= 0 {
				text = strings.TrimSpace(text[index+len("\n## My request:"):])
			}
		}
	}
	if strings.HasPrefix(text, "<user_query>") && strings.HasSuffix(text, "</user_query>") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<user_query>"), "</user_query>"))
	}
	return text
}

func setContentModerationUserEvidence(log *ContentModerationLog, protocol string, body []byte, fallback string, contextChars int) {
	user := moderationUserBody{}
	if messages := extractModerationUserBodies(protocol, body); len(messages) > 0 {
		user = messages[len(messages)-1]
	} else if len(body) == 0 {
		// Internal callers without a wire request can supply extracted user text.
		user.text = cleanModerationUserBody(fallback)
	}
	setContentModerationEvidence(log, user.text, contextChars)
	log.InputEvidence.ContentScope = moderationUserBodyScope
	log.InputEvidence.Source = user.source
}
