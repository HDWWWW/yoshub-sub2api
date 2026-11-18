package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIEarlyControlBytesKey = "openai_early_control_bytes"
const openAIEarlyControlMaxBytes = 64 * 1024

// Only leading, complete rate-limit status events may bypass first-output
// staging. Never reorder lifecycle events or release attempt IDs/turn state.
// Unknown, oversized or malformed frames fall back to the existing parser.
type openAIEarlyControlFilter struct {
	lines    []string
	bytes    int
	disabled bool
}

func (s *OpenAIGatewayService) newOpenAIEarlyControlFilter(c *gin.Context, account *Account) *openAIEarlyControlFilter {
	if s == nil ||
		account == nil || account.Platform != PlatformOpenAI || c == nil || c.Request == nil ||
		!strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
		return nil
	}
	if s.settingService == nil || !s.settingService.groupEarlyRateLimitsEnabled(c.Request.Context(), getOpenAIGroupIDFromContext(c)) {
		return nil
	}
	return &openAIEarlyControlFilter{}
}

// Push returns ordinary lines to the existing parser, or a complete original
// status frame to flush immediately. Once another event begins, filtering ends
// for this attempt, preserving upstream order without retaining more content.
func (f *openAIEarlyControlFilter) Push(line string) (ordinary []string, frame string) {
	if f == nil || f.disabled {
		return []string{line}, ""
	}
	if len(f.lines) == 0 {
		candidate := false
		if name, ok := extractOpenAISSEEventLine(line); ok {
			candidate = name == "codex.rate_limits"
		} else if data, ok := extractOpenAISSEDataLine(line); ok {
			candidate = gjson.Valid(data) && gjson.Get(data, "type").String() == "codex.rate_limits"
		}
		if !candidate {
			f.disabled = true
			return []string{line}, ""
		}
	}
	f.lines = append(f.lines, line)
	f.bytes += len(line) + 1
	if f.bytes > openAIEarlyControlMaxBytes {
		return f.Finish(), ""
	}
	if line != "" {
		return nil, ""
	}
	dataCount := 0
	for _, part := range f.lines[:len(f.lines)-1] {
		if name, ok := extractOpenAISSEEventLine(part); ok && name == "codex.rate_limits" {
			continue
		}
		if data, ok := extractOpenAISSEDataLine(part); ok && gjson.Valid(data) &&
			gjson.Get(data, "type").String() == "codex.rate_limits" {
			dataCount++
			continue
		}
		return f.Finish(), ""
	}
	if dataCount != 1 {
		return f.Finish(), ""
	}
	frame = strings.Join(f.lines, "\n") + "\n"
	f.lines, f.bytes = nil, 0
	return nil, frame
}

func (f *openAIEarlyControlFilter) Finish() []string {
	if f == nil {
		return nil
	}
	lines := f.lines
	f.lines, f.bytes, f.disabled = nil, 0, true
	return lines
}

func openAIEarlyControlBytes(c *gin.Context) int {
	if c == nil {
		return 0
	}
	value, _ := c.Get(openAIEarlyControlBytesKey)
	n, _ := value.(int)
	return n
}

// Serialize with the passthrough keepalive, without stopping its idle timer.
// These bytes commit HTTP 200 but are not model output: retries remain allowed,
// and final errors must use an SSE terminal rather than append JSON to the body.
func writeOpenAIEarlyControlFrame(c *gin.Context, frame string) error {
	w := c.Writer
	if value, ok := c.Get(openAICompactSSEKeepaliveKey); ok {
		if k, ok := value.(*openAICompactSSEKeepalive); ok && k != nil {
			k.mu.Lock()
			defer k.mu.Unlock()
			w = k.writer
			k.started = true
		}
	}
	n, err := io.WriteString(w, frame)
	c.Set(openAIEarlyControlBytesKey, openAIEarlyControlBytes(c)+n)
	if err != nil {
		MarkResponseCommitted(c) // A partial frame must never be followed by a replay.
		return err
	}
	if n != len(frame) {
		MarkResponseCommitted(c)
		return io.ErrShortWrite
	}
	w.Flush()
	return nil
}

// A final HTTP error takes over the writer. Only a real, locally emitted happy
// notification changes its wire format; a configured timer is not a commit.
func stopHappyNotificationForHTTPError(c *gin.Context) bool {
	if c == nil || smartWSHasDispatched(c) {
		return false
	}
	v, _ := c.Get(openAICompactSSEKeepaliveKey)
	k, _ := v.(*openAICompactSSEKeepalive)
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.markStoppedLocked()
	return k.initialEventSent
}

// body is the endpoint's public HTTP error envelope, not a raw WS frame.
func writeHappyNotificationHTTPError(c *gin.Context, status int, body []byte) {
	typ, code, msg := OpenAIUpstreamErrorFields(status, body)
	public := gin.H{"code": code, "type": typ, "message": msg, "status_code": status}
	if param := gjson.GetBytes(body, "error.param"); param.Type == gjson.String {
		public["param"] = param.Str
	}
	MarkResponseCommitted(c)
	writeOpenAIControlFailure(c, status, typ, msg, public)
}

// Later attempts can fail before reaching the stream handler (HTTP errors or
// local validation). Once a leading status committed SSE, every such error
// must terminate that same stream; an HTTP/JSON response is no longer valid.
func writeOpenAIEarlyControlError(c *gin.Context, status int, body []byte) bool {
	if openAIEarlyControlBytes(c) <= 0 {
		return false
	}
	StopOpenAICompactSSEKeepaliveCommitted(c)
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	}
	if code == "" {
		code = "upstream_error"
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message == "" {
		message = "Upstream request failed"
	}
	c.Header("Content-Type", "text/event-stream")
	MarkResponseCommitted(c)
	writeOpenAICompactSSEFailureMessage(c, status, code, message)
	return true
}

// A retry may return a unary Responses object after an earlier limits event
// committed SSE. Complete that stream without changing the upstream usage.
func writeOpenAIEarlyControlResponse(c *gin.Context, status int, body []byte) bool {
	if status >= 400 && stopHappyNotificationForHTTPError(c) {
		writeHappyNotificationHTTPError(c, status, body)
		return true
	}
	if openAIEarlyControlBytes(c) <= 0 {
		return false
	}
	if status < 200 || status >= 300 {
		return writeOpenAIEarlyControlError(c, status, body)
	}
	root := gjson.ParseBytes(body)
	if !gjson.ValidBytes(body) || !root.IsObject() || root.Get("id").String() == "" || !root.Get("output").IsArray() {
		return writeOpenAIEarlyControlError(c, http.StatusBadGateway, []byte(`{"error":{"code":"invalid_upstream_response","message":"Upstream returned an invalid Responses object"}}`))
	}
	var payload bytes.Buffer
	appendEvent := func(kind string, data map[string]any) {
		data["type"] = kind
		encoded, _ := json.Marshal(data) // Only valid RawMessage, strings and integers.
		_, _ = payload.WriteString("event: " + kind + "\ndata: ")
		_, _ = payload.Write(encoded)
		_, _ = payload.WriteString("\n\n")
	}
	for index, item := range root.Get("output").Array() {
		appendEvent("response.output_item.done", map[string]any{"output_index": index, "item": json.RawMessage(item.Raw)})
	}
	terminal := "response.completed"
	switch root.Get("status").String() {
	case "incomplete":
		terminal = "response.incomplete"
	case "failed", "cancelled", "canceled":
		terminal = "response.failed"
	}
	appendEvent(terminal, map[string]any{"response": json.RawMessage(body)})
	StopOpenAICompactSSEKeepaliveCommitted(c)
	MarkResponseCommitted(c)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.Write(payload.Bytes())
	c.Writer.Flush()
	return true
}

func writeOpenAIJSONError(c *gin.Context, status int, body any) {
	if status >= 400 && stopHappyNotificationForHTTPError(c) {
		payload, _ := json.Marshal(body)
		writeHappyNotificationHTTPError(c, status, payload)
		return
	}
	if openAIEarlyControlBytes(c) > 0 {
		payload, _ := json.Marshal(body)
		writeOpenAIEarlyControlError(c, status, payload)
		return
	}
	c.JSON(status, body)
}

// The passthrough scanner runs on the response-writing goroutine. Native
// streams instead apply Push in their main event loop, never in the reader
// goroutine, so all writes and metric updates stay serialized.
type openAIEarlyControlScanner struct {
	source interface {
		Scan() bool
		Text() string
		Err() error
	}
	filter *openAIEarlyControlFilter
	emit   func(string)
	queue  []string
	text   string
	done   bool
}

func (s *openAIEarlyControlScanner) Scan() bool {
	if len(s.queue) == 0 && !s.done && (s.filter == nil || s.filter.disabled) {
		if !s.source.Scan() {
			s.done = true
			return false
		}
		s.text = s.source.Text()
		return true
	}
	for len(s.queue) == 0 && !s.done {
		if !s.source.Scan() {
			s.done = true
			s.queue = s.filter.Finish()
			break
		}
		var frame string
		s.queue, frame = s.filter.Push(s.source.Text())
		if frame != "" {
			s.emit(frame)
		}
	}
	if len(s.queue) == 0 {
		return false
	}
	s.text, s.queue = s.queue[0], s.queue[1:]
	return true
}

func (s *openAIEarlyControlScanner) Text() string { return s.text }
func (s *openAIEarlyControlScanner) Err() error   { return s.source.Err() }
