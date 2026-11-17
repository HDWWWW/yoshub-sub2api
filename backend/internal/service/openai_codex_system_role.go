package service

import (
	"fmt"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	normalized := body
	changed := false
	var patchErr error
	input.ForEach(func(index, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		if typ := item.Get("type"); typ.Exists() && (typ.Type != gjson.String || typ.Str != "message") {
			return true
		}
		if role := item.Get("role"); role.Type != gjson.String || role.Str != "system" {
			return true
		}
		normalized, patchErr = sjson.SetBytes(normalized, "input."+strconv.FormatInt(index.Int(), 10)+".role", "developer")
		if patchErr != nil {
			return false
		}
		changed = true
		return true
	})
	if patchErr != nil {
		return body, false, fmt.Errorf("normalize Codex system message role: %w", patchErr)
	}
	return normalized, changed, nil
}
