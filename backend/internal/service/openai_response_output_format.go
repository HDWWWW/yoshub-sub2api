package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIJSONObjectInstruction = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"Return a valid JSON object."}]}`

type openAIOutputFormatValidationError struct {
	param   string
	message string
}

func (e *openAIOutputFormatValidationError) Error() string {
	return fmt.Sprintf("Invalid '%s': %s", e.param, e.message)
}

func outputFormatError(param, message string) error {
	return &openAIOutputFormatValidationError{param: param, message: message}
}

func writeOpenAIOutputFormatError(c *gin.Context, err error) {
	param := "text.format"
	if validation, ok := err.(*openAIOutputFormatValidationError); ok {
		param = validation.param
	}
	detail := gin.H{
		"type": "invalid_request_error", "message": err.Error(), "param": param,
	}
	stopHappyNotificationForHTTPError(c)
	StopOpenAICompactSSEKeepaliveCommitted(c)
	if c.Writer.Written() && strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		MarkResponseCommitted(c)
		if c.Request != nil && strings.HasSuffix(c.Request.URL.Path, "/chat/completions") {
			MarkOpsStreamError(c, "invalid_request_error", err.Error(), http.StatusBadRequest)
			payload, _ := json.Marshal(gin.H{"error": detail})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", payload)
			c.Writer.Flush()
		} else {
			writeOpenAIControlFailure(c, http.StatusBadRequest, "invalid_request_error", err.Error(), detail)
		}
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": detail})
}

// Run before transport selection so HTTP, passthrough and SmartWS use the same
// output contract. Patch only the format/name or input, preserving opaque data.
func normalizeOpenAIResponseOutputFormat(body []byte) ([]byte, bool, error) {
	format := parseRawJSONView(body).Get("text.format")
	switch strings.ToLower(strings.TrimSpace(format.Get("type").String())) {
	case "json_object":
		return ensureOpenAIJSONObjectInstruction(body)
	case "json_schema":
		var definition map[string]any
		if err := decodeOpenAIJSONUseNumber([]byte(format.Raw), &definition); err != nil {
			return body, false, outputFormatError("text.format", "must be an object")
		}
		schema, ok := definition["schema"].(map[string]any)
		if !ok {
			return body, false, outputFormatError("text.format.schema", "must be an object")
		}
		// Output schema strictness is opt-in. Unlike function-tool defaults,
		// an omitted or null text.format.strict must not impose constraints.
		strict := false
		if value := definition["strict"]; value != nil {
			var valid bool
			strict, valid = value.(bool)
			if !valid {
				return body, false, outputFormatError("text.format.strict", "must be a boolean")
			}
		}
		if strict {
			budget := 10000
			if err := validateOpenAIStrictOutputSchema(schema, "text.format.schema", 0, &budget); err != nil {
				return body, false, err
			}
		}
		name, exists := definition["name"]
		if !exists || name == nil || name == "" {
			next, err := sjson.SetBytes(body, "text.format.name", "response_schema")
			return next, err == nil, err
		}
		text, ok := name.(string)
		if !ok || len(text) > 64 || strings.ContainsFunc(text, func(r rune) bool {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return false
			default:
				return r != '_' && r != '-'
			}
		}) {
			return body, false, outputFormatError("text.format.name", "must contain 1-64 letters, digits, underscores or hyphens")
		}
	}
	return body, false, nil
}

func ensureOpenAIJSONObjectInstruction(body []byte) ([]byte, bool, error) {
	input := parseRawJSONView(body).Get("input")
	var items []string
	switch {
	case input.Type == gjson.String:
		if strings.TrimSpace(input.Str) == "" || strings.Contains(strings.ToLower(input.Str), "json") {
			return body, false, nil
		}
		items = []string{openAIJSONObjectInstruction, `{"role":"user","content":` + input.Raw + `}`}
	case input.IsArray():
		hasInstruction := false
		input.ForEach(func(_, item gjson.Result) bool {
			if openAIMessageHasJSONInstruction(item) {
				hasInstruction = true
				return false
			}
			items = append(items, item.Raw)
			return true
		})
		if hasInstruction || len(items) == 0 {
			return body, false, nil
		}
		items = append([]string{openAIJSONObjectInstruction}, items...)
	default:
		// Do not manufacture input for an empty or malformed business request.
		return body, false, nil
	}
	return replaceOpenAIRawInput(body, input, items), true, nil
}

func openAIMessageHasJSONInstruction(item gjson.Result) bool {
	if kind := item.Get("type").String(); kind != "" && kind != "message" {
		return false
	}
	switch item.Get("role").String() {
	case "system", "developer", "user":
	default:
		return false
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		return strings.Contains(strings.ToLower(content.Str), "json")
	}
	found := false
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			if kind := part.Get("type").String(); kind == "input_text" || kind == "text" {
				text := part.Get("text")
				found = text.Type == gjson.String && strings.Contains(strings.ToLower(text.Str), "json")
			}
			return !found
		})
	}
	return found
}

// This is a preflight for strict object constraints, not a general JSON Schema
// validator. Never change optional fields, dynamic properties, or instance data.
func validateOpenAIStrictOutputSchema(schema map[string]any, path string, depth int, budget *int) error {
	*budget--
	if depth > 64 || *budget < 0 {
		return outputFormatError(path, "schema validation limit exceeded")
	}
	properties, hasProperties := schema["properties"].(map[string]any)
	isObject := schema["type"] == "object" || schema["type"] == nil && hasProperties
	if union, ok := schema["type"].([]any); ok {
		for _, kind := range union {
			isObject = isObject || kind == "object"
		}
	}
	if isObject {
		if closed, ok := schema["additionalProperties"].(bool); !ok || closed {
			return outputFormatError(path+".additionalProperties", "must be false for strict output; dynamic properties cannot be silently removed")
		}
		rawRequired, hasRequired := schema["required"]
		required, ok := rawRequired.([]any)
		if !ok && (hasRequired || len(properties) > 0) {
			return outputFormatError(path+".required", "must be an array containing every property for strict output")
		}
		seen := make(map[string]bool, len(required))
		for _, value := range required {
			name, ok := value.(string)
			if !ok || seen[name] {
				return outputFormatError(path+".required", "must contain unique property names")
			}
			if _, exists := properties[name]; !exists {
				return outputFormatError(path+".required", fmt.Sprintf("unknown property %q", name))
			}
			seen[name] = true
		}
		for _, name := range sortedOutputSchemaKeys(properties) {
			if !seen[name] {
				return outputFormatError(path+".required", fmt.Sprintf("missing property %q; optional fields cannot be silently made required", name))
			}
		}
	}
	for _, keyword := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"} {
		children, _ := schema[keyword].(map[string]any)
		for _, key := range sortedOutputSchemaKeys(children) {
			if child, ok := children[key].(map[string]any); ok {
				if err := validateOpenAIStrictOutputSchema(child, path+"."+keyword+"["+fmt.Sprintf("%q", key)+"]", depth+1, budget); err != nil {
					return err
				}
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties", "not", "if", "then", "else", "contains"} {
		if child, ok := schema[keyword].(map[string]any); ok {
			if err := validateOpenAIStrictOutputSchema(child, path+"."+keyword, depth+1, budget); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{"anyOf", "allOf", "oneOf", "prefixItems", "items"} {
		children, _ := schema[keyword].([]any)
		for index, value := range children {
			if child, ok := value.(map[string]any); ok {
				if err := validateOpenAIStrictOutputSchema(child, fmt.Sprintf("%s.%s[%d]", path, keyword, index), depth+1, budget); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sortedOutputSchemaKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
