package service

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// Match Codex's system-to-developer role compatibility without extracting or
// moving messages. Preserve instructions and continuation history; a full Codex
// transform would change those semantics.
func normalizeOpenAICodexSystemRolesBody(body []byte, account *Account) ([]byte, bool, error) {
	if !account.IsOpenAIOAuthLike() {
		return body, false, nil
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}
	// Collect original token offsets in array order. An escaped "system" can
	// be longer than its replacement, so use raw lengths to size the result.
	const replacement = `"developer"`
	type span struct{ start, end int }
	spans := make([]span, 0, 8)
	size, lastEnd := len(body), 0
	var patchErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		if typ := item.Get("type"); typ.Exists() && (typ.Type != gjson.String || typ.Str != "message") {
			return true
		}
		role := item.Get("role")
		if role.Type != gjson.String || role.Str != "system" {
			return true
		}
		start, end := role.Index, role.Index+len(role.Raw)
		if start < lastEnd || start > len(body)-len(role.Raw) || string(body[start:end]) != role.Raw {
			patchErr = fmt.Errorf("normalize Codex system message role: invalid source span at byte %d", start)
			return false
		}
		spans = append(spans, span{start, end})
		size += len(replacement) - len(role.Raw)
		lastEnd = end
		return true
	})
	if patchErr != nil {
		return body, false, patchErr
	}
	if len(spans) == 0 {
		return body, false, nil
	}

	out := make([]byte, 0, size)
	cursor := 0
	for _, s := range spans {
		out = append(out, body[cursor:s.start]...)
		out = append(out, replacement...)
		cursor = s.end
	}
	return append(out, body[cursor:]...), true, nil
}
