package service

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponseOutputFormatJSONObject(t *testing.T) {
	for _, tc := range []struct {
		name, input, extra string
		changed            bool
	}{
		{"string", `"hello"`, "", true},
		{"string instruction", `"Return JSON."`, "", false},
		{"instructions only", `[{"role":"user","content":"hello"}]`, `,"instructions":"Return JSON with a name."`, true},
		{"system instruction", `[{"role":"system","content":"Return JSON."},{"role":"user","content":"hello"}]`, "", false},
		{"developer instruction", `[{"role":"developer","content":[{"type":"input_text","text":"Return jSoN."}]}]`, "", false},
		{"user text", `[{"role":"user","content":[{"type":"text","text":"Return JSON."}]}]`, "", false},
		{"opaque tool output", `[{"type":"function_call_output","call_id":"call_1","output":"json"},{"role":"user","content":"summarize"}]`, "", true},
		{"image URL", `[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/json.png"}]}]`, "", true},
		{"escaped instruction", `[{"role":"user","content":"Return \u004aSON."}]`, "", false},
		{"metadata", `[{"role":"user","content":"hello","metadata":{"text":"JSON"}}]`, "", true},
		{"empty input", `[]`, "", false},
		{"blank input", `" "`, "", false},
		{"null input", `null`, "", false},
		{"invalid input", `{}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"text":{"format":{"type":"json_object"}},"input":` + tc.input + tc.extra + `,"service_tier":"priority","counter":900719925474099312345,"marker":"\u0061"}`)
			original := bytes.Clone(body)
			next, changed, err := normalizeOpenAIResponseOutputFormat(body)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, original, body)
			require.True(t, json.Valid(next))
			for _, path := range []string{"text", "instructions", "service_tier", "counter", "marker"} {
				require.Equal(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(next, path).Raw, path)
			}
			if changed {
				require.JSONEq(t, openAIJSONObjectInstruction, gjson.GetBytes(next, "input.0").Raw)
				if gjson.Parse(tc.input).IsArray() {
					for i, item := range gjson.Parse(tc.input).Array() {
						require.Equal(t, item.Raw, gjson.GetBytes(next, "input").Array()[i+1].Raw)
					}
				} else {
					require.Equal(t, gjson.Parse(tc.input).Str, gjson.GetBytes(next, "input.1.content").Str)
				}
			} else {
				require.Equal(t, body, next)
			}
			again, changed, err := normalizeOpenAIResponseOutputFormat(next)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, next, again)
		})
	}
}

func TestResponseOutputFormatDoesNotChangeOtherRequests(t *testing.T) {
	for _, body := range []string{
		`{"input":"hello"}`, `{"input":"hello","text":{"format":{"type":"text"}}}`,
		`{"text":{"format":{"type":"json_object"}}}`, `{"input":"hello","response_format":{"type":"json_object"}}`,
	} {
		next, changed, err := normalizeOpenAIResponseOutputFormat([]byte(body))
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, string(next))
	}
}

func TestResponseOutputFormatSchema(t *testing.T) {
	const valid = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`
	for _, tc := range []struct {
		name, format, param string
		changed             bool
	}{
		{"name default", `"schema":` + valid, "", true},
		{"keep name", `"name":"my_schema","schema":` + valid, "", false},
		{"empty name", `"name":"","schema":` + valid, "", true},
		{"empty object", `"schema":{"type":"object","additionalProperties":false}`, "", true},
		{"bad name", `"name":"bad name","schema":` + valid, "text.format.name", false},
		{"name type", `"name":{},"schema":` + valid, "text.format.name", false},
		{"missing schema", `"name":"a"`, "text.format.schema", false},
		{"bad strict", `"strict":"false","schema":` + valid, "text.format.strict", false},
		{"non strict optional", `"strict":false,"schema":{"type":"object","properties":{"optional":{"type":"string"}},"additionalProperties":true}`, "", true},
		{"strict optional", `"strict":true,"schema":{"type":"object","properties":{"optional":{"type":"string"}},"required":[],"additionalProperties":false}`, "text.format.schema.required", false},
		{"default non strict", `"schema":{"type":"object","properties":{"optional":{"type":"string"}},"required":[],"additionalProperties":true}`, "", true},
		{"null non strict", `"strict":null,"schema":{"type":"object","properties":{"optional":{"type":"string"}},"additionalProperties":true}`, "", true},
		{"missing additional properties", `"strict":true,"schema":{"type":"object","properties":{},"required":[]}`, "text.format.schema.additionalProperties", false},
		{"dynamic properties", `"strict":true,"schema":{"type":"object","properties":{},"required":[],"additionalProperties":{"type":"string"}}`, "text.format.schema.additionalProperties", false},
		{"unknown required", `"strict":true,"schema":{"type":"object","properties":{},"required":["unknown"],"additionalProperties":false}`, "text.format.schema.required", false},
		{"bad required", `"strict":true,"schema":{"type":"object","properties":{},"required":[1],"additionalProperties":false}`, "text.format.schema.required", false},
		{"nullable object", `"strict":true,"schema":{"type":["object","null"],"properties":{"a":{"type":"string"}},"required":[],"additionalProperties":false}`, "text.format.schema.required", false},
		{"array item", `"strict":true,"schema":{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{},"required":[]}}},"required":["rows"],"additionalProperties":false}`, `text.format.schema.properties["rows"].items.additionalProperties`, false},
		{"defs", `"strict":true,"schema":{"$ref":"#/$defs/entry","$defs":{"entry":{"type":"object","properties":{"a":{"type":"string"}},"required":[],"additionalProperties":false}}}`, `text.format.schema.$defs["entry"].required`, false},
		{"anyOf", `"strict":true,"schema":{"anyOf":[{"type":"object","properties":{},"required":[]}]}`, "text.format.schema.anyOf[0].additionalProperties", false},
		{"instance values untouched", `"strict":true,"schema":{"type":"object","properties":{"a":{"type":"string","default":{"type":"object"},"examples":[{"type":"object"}],"enum":["a","b"]}},"required":["a"],"additionalProperties":false}`, "", true},
		{"non object properties ignored", `"strict":true,"schema":{"type":"object","properties":{"a":{"type":"string","properties":{"ignored":{"type":"string"}}}},"required":["a"],"additionalProperties":false}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.1","input":"hello","counter":900719925474099312345,"text":{"format":{"type":"json_schema",` + tc.format + `}}}`)
			original := bytes.Clone(body)
			next, changed, err := normalizeOpenAIResponseOutputFormat(body)
			require.Equal(t, original, body)
			if tc.param != "" {
				var validation *openAIOutputFormatValidationError
				require.ErrorAs(t, err, &validation)
				require.Equal(t, tc.param, validation.param)
				require.Equal(t, body, next)
				require.False(t, changed)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, gjson.GetBytes(body, "text.format.schema").Raw, gjson.GetBytes(next, "text.format.schema").Raw)
			require.Equal(t, gjson.GetBytes(body, "text.format.strict").Raw, gjson.GetBytes(next, "text.format.strict").Raw)
			require.Equal(t, "900719925474099312345", gjson.GetBytes(next, "counter").Raw)
			if changed {
				require.Equal(t, "response_schema", gjson.GetBytes(next, "text.format.name").Str)
			}
			again, changed, err := normalizeOpenAIResponseOutputFormat(next)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, next, again)
		})
	}
}

func TestResponseOutputFormatSchemaDepthBound(t *testing.T) {
	schema := `{"type":"string"}`
	for i := 0; i < 66; i++ {
		schema = `{"type":"array","items":` + schema + `}`
	}
	body := []byte(`{"text":{"format":{"type":"json_schema","strict":true,"schema":` + schema + `}}}`)
	next, changed, err := normalizeOpenAIResponseOutputFormat(body)
	require.ErrorContains(t, err, "schema validation limit exceeded")
	require.Equal(t, body, next)
	require.False(t, changed)
}

func TestResponseOutputFormatWSCompatibilityScope(t *testing.T) {
	body := []byte(`{"type":"response.create","input":[{"role":"user","content":"hello"}],"text":{"format":{"type":"json_object"}}}`)
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformAnthropic} {
		for _, compact := range []bool{false, true} {
			next, _, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, &Account{Platform: platform, Type: AccountTypeAPIKey}, openAIResponsesCompatibilityOptions{Compact: compact})
			require.NoError(t, err)
			if platform == PlatformOpenAI && !compact {
				require.Equal(t, "developer", gjson.GetBytes(next, "input.0.role").Str)
			} else {
				require.Equal(t, body, next)
			}
		}
	}
}

func TestResponseOutputFormatErrorAfterSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", path, nil)
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte(":\n\n"))
		c.Writer.Flush()
		writeOpenAIOutputFormatError(c, outputFormatError("text.format.schema.required", "missing property"))
		wire := rec.Body.String()
		require.Contains(t, wire, `"param":"text.format.schema.required"`)
		require.NotContains(t, wire, "\n{", "must not append bare JSON after SSE")
		if strings.HasSuffix(path, "/responses") {
			require.Contains(t, wire, "event: response.failed")
		} else {
			require.Contains(t, wire, "data: [DONE]")
		}
	}
}
