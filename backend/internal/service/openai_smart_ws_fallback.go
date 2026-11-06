package service

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Observe the original HTTP response while its formatter reads it. A bare
// error can collect only a bounded failed tail, as on the WS path. The normal
// formatter owns output, cancellation and HTTP usage attribution.
type smartWSFallbackBody struct {
	io.ReadCloser
	headers              http.Header
	request              []byte
	c                    *gin.Context
	dispatch             *smartWSDispatch
	observation          *smartWSObservation
	limit, documentLimit int64
	sse                  bool
	formatKnown          bool
	line, data, document []byte
	eventName            string
	readErr              error
	failure              []byte
	responseID           string
	tailFrames           int
	terminal, tailRead   bool
}

func newSmartWSFallbackBody(source io.ReadCloser, headers http.Header, request []byte, c *gin.Context, dispatch *smartWSDispatch, observation *smartWSObservation, limit, documentLimit int64) io.ReadCloser {
	if limit <= 0 {
		limit = openAIWSMessageReadLimitBytes
	}
	return &smartWSFallbackBody{ReadCloser: source, headers: headers.Clone(), request: request, c: c,
		dispatch: dispatch, observation: observation, limit: limit, documentLimit: documentLimit,
		formatKnown: strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream"),
		sse:         strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")}
}

func (b *smartWSFallbackBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.tailRead {
		return 0, io.EOF
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	n, err := b.ReadCloser.Read(p)
	if !b.terminal {
		b.observeBytes(p[:n])
		if err == io.EOF && !b.terminal {
			if b.sse {
				b.observeLine()
				b.observeEvent()
			} else {
				b.observePayload(b.document)
			}
		}
	}
	if b.readErr != nil {
		return n, b.readErr
	}
	if len(b.failure) > 0 && !b.terminal && err == nil {
		b.collectFailedTail()
	}
	return n, err
}

func (b *smartWSFallbackBody) collectFailedTail() {
	// Closing this request body interrupts a pending read without canceling
	// another request sharing its HTTP connection. No generation is sent.
	timer := time.AfterFunc(200*time.Millisecond, func() { _ = b.ReadCloser.Close() })
	defer timer.Stop()
	defer b.ReadCloser.Close()
	buffer := make([]byte, 32*1024)
	for !b.terminal && b.readErr == nil {
		n, err := b.ReadCloser.Read(buffer)
		b.observeBytes(buffer[:n])
		if err != nil {
			break
		}
	}
	b.tailRead = true
}

func (b *smartWSFallbackBody) observeBytes(raw []byte) {
	if !b.formatKnown {
		start := bytes.TrimSpace(raw)
		if len(start) == 0 {
			return
		}
		b.formatKnown = true
		b.sse = start[0] != '{' && start[0] != '['
	}
	if !b.sse {
		if int64(len(b.document)+len(raw)) <= b.documentLimit {
			b.document = append(b.document, raw...)
		} else {
			b.readErr, b.document = ErrUpstreamResponseBodyTooLarge, nil
		}
		return
	}
	for len(raw) > 0 && !b.terminal {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			end = len(raw)
		}
		if int64(len(b.line)+len(b.data)+end) > b.limit {
			b.readErr, b.line, b.data = ErrUpstreamResponseBodyTooLarge, nil, nil
			return
		}
		b.line = append(b.line, raw[:end]...)
		if end == len(raw) {
			break
		}
		b.observeLine()
		raw = raw[end+1:]
	}
}

func (b *smartWSFallbackBody) observeLine() {
	line := bytes.TrimSuffix(b.line, []byte{'\r'})
	b.line = nil
	if len(line) == 0 {
		b.observeEvent()
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		b.eventName = strings.TrimSpace(string(line[6:]))
	} else if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line[5:], []byte{' '})
		b.data = append(b.data, data...)
		b.data = append(b.data, '\n')
	}
}

func (b *smartWSFallbackBody) observeEvent() {
	if len(b.data) > 0 {
		payload := []byte(openAICompatPayloadWithEventType(strings.TrimSpace(string(b.data)), b.eventName))
		b.observePayload(payload)
	}
	b.data, b.eventName = nil, ""
}

func smartWSObservedFallbackError(c *gin.Context) error {
	if c == nil {
		return nil
	}
	v, _ := c.Get(smartWSHTTPFailureKey)
	if failure, ok := v.(*smartWSHTTPFailure); ok && failure.httpFallback {
		return smartWSStopFailure(openAIHTTPWSEventErrorStatus(failure.payload), failure.payload, failure.headers)
	}
	return nil
}

func (b *smartWSFallbackBody) observePayload(payload []byte) {
	if !gjson.ValidBytes(payload) {
		return
	}
	original := payload
	typ := gjson.GetBytes(payload, "type").String()
	if typ == "" || typ == "response" {
		status := gjson.GetBytes(payload, "status").String()
		if status == "" && gjson.GetBytes(payload, "error").IsObject() {
			status = "failed"
		}
		switch status {
		case "completed", "incomplete", "failed", "cancelled", "canceled":
			typ = "response." + status
			payload = append(append([]byte(`{"type":"`+typ+`","response":`), payload...), '}')
		default:
			return
		}
	}
	id := smartWSResponseID(payload)
	if len(b.failure) > 0 {
		b.tailFrames++
		if b.tailFrames > 8 || (id != "" && b.responseID != "" && id != b.responseID) || typ == "response.created" ||
			(isOpenAIWSTerminalEvent(typ) && typ != "response.failed") {
			b.terminal = true
			return
		}
		if typ != "response.failed" {
			return
		}
		b.observation.event(payload)
		b.rememberFailure(b.failure, payload)
		b.terminal = true
		return
	}
	if id != "" {
		b.responseID = id
	}
	b.observation.event(payload)
	failed := typ == "error" || typ == "response.failed" || gjson.GetBytes(payload, "response.status").String() == "failed"
	if failed || isOpenAIWSTerminalEvent(typ) {
		b.dispatch.state.Store(smartWSTerminal)
		b.terminal = typ != "error"
	}
	if failed {
		b.failure = append([]byte(nil), original...)
		b.rememberFailure(b.failure, nil)
	}
}

func (b *smartWSFallbackBody) rememberFailure(payload, tail []byte) {
	rememberSmartWSHTTPFailure(b.c, b.headers, b.request, payload, tail, b.observation.start)
	v, _ := b.c.Get(smartWSHTTPFailureKey)
	if failure, ok := v.(*smartWSHTTPFailure); ok {
		failure.httpFallback = true
	}
}
