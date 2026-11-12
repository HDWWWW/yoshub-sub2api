package service

import (
	"bufio"
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
	line, data           []byte
	eventName            string
	readErr              error
	reader               *bufio.Reader
	pending              []byte
	failure              []byte
	responseID           string
	tailFrames           int
	terminal, tailRead   bool
}

func newSmartWSFallbackBody(source io.ReadCloser, headers http.Header, request []byte, c *gin.Context, dispatch *smartWSDispatch, observation *smartWSObservation, limit, documentLimit int64) io.ReadCloser {
	if limit <= 0 {
		limit = openAIWSMessageReadLimitBytes
	}
	if documentLimit <= 0 {
		documentLimit = defaultUpstreamResponseReadMaxBytes
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
	if len(b.pending) == 0 {
		if b.readErr != nil {
			return 0, b.readErr
		}
		if b.tailRead {
			return 0, io.EOF
		}
		frame, err := b.readFrame()
		if b.readErr != nil {
			err = b.readErr
		}
		b.readErr = err
		b.pending = frame
		if len(b.failure) > 0 && !b.terminal && err == nil {
			b.collectFailedTail()
		}
		if len(b.pending) == 0 {
			return 0, b.readErr
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// Hold at most one bounded SSE event before handing it to a formatter. In
// particular, [DONE] must never reach a formatter without a Responses terminal:
// several legacy formatters treat that sentinel alone as successful completion.
func (b *smartWSFallbackBody) readFrame() ([]byte, error) {
	if b.reader == nil {
		b.reader = bufio.NewReader(b.ReadCloser)
	}
	if !b.formatKnown {
		var prefix []byte
		for {
			ch, err := b.reader.ReadByte()
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
			prefix = append(prefix, ch)
			if int64(len(prefix)) > b.documentLimit {
				return nil, ErrUpstreamResponseBodyTooLarge
			}
			if ch != ' ' && ch != '\t' && ch != '\r' && ch != '\n' {
				b.formatKnown, b.sse = true, ch != '{' && ch != '['
				break
			}
		}
		b.reader = bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), b.reader))
	}
	if !b.sse {
		body, err := io.ReadAll(io.LimitReader(b.reader, b.documentLimit+1))
		if int64(len(body)) > b.documentLimit {
			return nil, ErrUpstreamResponseBodyTooLarge
		}
		b.observePayload(body)
		if err != nil {
			return nil, err
		}
		if !b.terminal && len(b.failure) == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return body, io.EOF
	}
	var frame []byte
	partialLine := false
	for {
		line, err := b.reader.ReadSlice('\n')
		if int64(len(frame)+len(line)) > b.limit {
			return nil, ErrUpstreamResponseBodyTooLarge
		}
		frame = append(frame, line...)
		b.observeBytes(line)
		if b.readErr != nil {
			return nil, b.readErr
		}
		if err == bufio.ErrBufferFull {
			partialLine = true
			continue
		}
		if err != nil {
			if !b.terminal {
				b.observeLine()
				b.observeEvent()
			}
			if b.readErr != nil {
				return nil, b.readErr
			}
			if err == io.EOF && !b.terminal && len(b.failure) == 0 {
				err = io.ErrUnexpectedEOF
			}
			return frame, err
		}
		if !partialLine && (bytes.Equal(line, []byte{'\n'}) || bytes.Equal(line, []byte{'\r', '\n'})) {
			return frame, nil
		}
		partialLine = false
	}
}

func (b *smartWSFallbackBody) collectFailedTail() {
	// Closing this request body interrupts a pending read without canceling
	// another request sharing its HTTP connection. No generation is sent.
	timer := time.AfterFunc(200*time.Millisecond, func() { _ = b.Close() })
	defer timer.Stop()
	defer func() { _ = b.Close() }()
	buffer := make([]byte, 32*1024)
	for !b.terminal && b.readErr == nil {
		n, err := b.reader.Read(buffer)
		b.observeBytes(buffer[:n])
		if err != nil {
			break
		}
	}
	b.tailRead = true
}

func (b *smartWSFallbackBody) observeBytes(raw []byte) {
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

func smartWSBackgroundAccepted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, _ := c.Get(smartWSObservationKey)
	observation, _ := v.(*smartWSObservation)
	if observation == nil {
		return false
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.terminal == "background.accepted"
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
		data := strings.TrimSpace(string(b.data))
		if data == "[DONE]" && !b.terminal && len(b.failure) == 0 {
			b.readErr = io.ErrUnexpectedEOF
			return
		}
		payload := []byte(openAICompatPayloadWithEventType(data, b.eventName))
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
	if len(b.failure) == 0 {
		rememberSmartWSHTTPUsage(b.c, b.headers, b.request, payload, b.observation.start, false)
	}
	typ := gjson.GetBytes(payload, "type").String()
	if typ == "" || typ == "response" {
		status := gjson.GetBytes(payload, "status").String()
		if status == "" && gjson.GetBytes(payload, "error").IsObject() {
			status = "failed"
		}
		switch status {
		case "queued", "in_progress":
			// A non-streaming background POST acknowledges a job; it does not
			// promise a generation terminal or belong in completed latency samples.
			if !b.sse && gjson.GetBytes(b.request, "background").Bool() && !gjson.GetBytes(b.request, "stream").Bool() &&
				gjson.GetBytes(payload, "id").String() != "" {
				b.terminal = true
				b.dispatch.state.Store(smartWSTerminal)
				b.observation.mu.Lock()
				b.observation.terminal = "background.accepted"
				b.observation.mu.Unlock()
			}
			return
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
		rememberSmartWSHTTPUsage(b.c, b.headers, b.request, payload, b.observation.start, false)
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
