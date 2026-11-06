package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (s *AccountTestService) testExcelBPSConnection(c *gin.Context, account *Account, model, prompt, mode string, opts ...AccountTestOptions) error {
	if s.openaiGatewayService == nil {
		return s.sendErrorAndEnd(c, "Excel BPS gateway is unavailable")
	}
	if mode == AccountTestModeCompact || isOpenAIImageModel(account.GetMappedModel(model)) {
		return s.sendErrorAndEnd(c, "Use a text model for the Excel BPS connectivity test; image generation is available through the images API")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "hi"
	}
	payload := map[string]any{"model": model, "input": prompt, "stream": true, "max_output_tokens": 128}
	if effort := firstAccountTestOptions(opts).ReasoningEffort; effort != "" {
		payload["reasoning"] = map[string]any{"effort": effort}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	w := &excelBPSTestWriter{ResponseWriter: c.Writer, header: make(http.Header)}
	// Emit directly to the saved writer, never re-enter the adapter.
	w.emit = func(event TestEvent) {
		data, _ := json.Marshal(event)
		_, _ = w.ResponseWriter.WriteString("data: " + string(data) + "\n\n")
		w.ResponseWriter.Flush()
	}
	c.Writer = w
	_, err = s.openaiGatewayService.Forward(c.Request.Context(), c, account, body)
	c.Writer = w.ResponseWriter
	if err != nil {
		message := w.failure
		if message == "" {
			message = err.Error()
		}
		return s.sendErrorAndEnd(c, message)
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

type excelBPSTestWriter struct {
	gin.ResponseWriter
	header  http.Header
	pending []byte
	failure string
	written bool
	sawText bool
	emit    func(TestEvent)
}

func (w *excelBPSTestWriter) Header() http.Header               { return w.header }
func (w *excelBPSTestWriter) WriteHeader(int)                   {}
func (w *excelBPSTestWriter) WriteHeaderNow()                   { w.written = true }
func (w *excelBPSTestWriter) Written() bool                     { return w.written }
func (w *excelBPSTestWriter) Flush()                            { w.ResponseWriter.Flush() }
func (w *excelBPSTestWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *excelBPSTestWriter) Write(p []byte) (int, error) {
	w.written = true
	if !strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream") {
		w.failure = gjson.GetBytes(p, "error.message").String()
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	if len(w.pending) > 16<<20 {
		return 0, fmt.Errorf("Excel BPS test event exceeds limit")
	}
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			break
		}
		line := strings.TrimSpace(string(w.pending[:end]))
		w.pending = w.pending[end+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		switch gjson.Get(payload, "type").String() {
		case "response.output_text.delta":
			text := gjson.Get(payload, "delta").String()
			if text != "" {
				w.sawText = true
				w.emit(TestEvent{Type: "content", Text: text})
			}
		case "response.completed":
			if !w.sawText {
				for _, item := range gjson.Get(payload, "response.output").Array() {
					for _, content := range item.Get("content").Array() {
						if text := content.Get("text").String(); text != "" {
							w.emit(TestEvent{Type: "content", Text: text})
						}
					}
				}
			}
		case "response.failed", "error":
			w.failure = gjson.Get(payload, "response.error.message").String()
		}
	}
	return len(p), nil
}
