package smartws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

var eventTypePattern = regexp.MustCompile("^[A-Za-z0-9_.-]+$")

type eventBody struct {
	ctx                          context.Context
	pool                         *Pool
	connection                   *connection
	lease                        *lease
	readMu                       sync.Mutex
	mu                           sync.Mutex
	buffer                       []byte
	terminal, successful, closed bool
	responseID                   string
	previousID                   string // immutable for this lease
	stop                         chan struct{}
	once                         sync.Once
}

func newEventBody(ctx context.Context, p *Pool, c *connection, l *lease) *eventBody {
	p.mu.Lock()
	previousID := c.lastResponseID
	p.mu.Unlock()
	b := &eventBody{ctx: ctx, pool: p, connection: c, lease: l, previousID: previousID, stop: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = b.Close()
		case <-b.stop:
		}
	}()
	return b
}
func (b *eventBody) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		reuse := b.terminal && b.successful && len(b.buffer) == 0 && b.ctx.Err() == nil
		id := b.responseID
		b.mu.Unlock()
		close(b.stop)
		b.pool.release(b.connection, b.lease, reuse, id)
	})
	return nil
}
func (b *eventBody) failure(reason string, transportFailure bool, cause ...error) (int, error) {
	if err := b.ctx.Err(); err != nil {
		_ = b.Close()
		b.pool.count("cancelled")
		return 0, &SentError{Err: err}
	}
	b.mu.Lock()
	b.successful = false
	b.mu.Unlock()
	b.pool.count("stream_failed")
	switch size := b.lease.requestBytes; {
	case size <= 64<<10:
		b.pool.count("stream_failure_request_le_64k")
	case size <= 256<<10:
		b.pool.count("stream_failure_request_le_256k")
	case size <= 1<<20:
		b.pool.count("stream_failure_request_le_1m")
	case size <= 4<<20:
		b.pool.count("stream_failure_request_le_4m")
	default:
		b.pool.count("stream_failure_request_le_8m")
	}

	codes := map[string]string{
		"upstream read timeout":                  "read_timeout",
		"request context cancelled":              "context_cancelled",
		"upstream stream closed":                 "lease_closed",
		"invalid upstream JSON event":            "invalid_json",
		"invalid upstream event type":            "invalid_event_type",
		"conflicting upstream response IDs":      "conflicting_response_id",
		"unexpected upstream response.created":   "unexpected_created",
		"stale upstream response":                "stale_response",
		"unattributed upstream event":            "unattributed_event",
		"upstream event before response.created": "before_created",
		"upstream response ID mismatch":          "response_id_mismatch",
		"invalid completed response":             "invalid_completed",
	}
	code := codes[reason]
	if code == "" {
		code = "connection_interrupted"
	}
	b.pool.count("stream_failure_" + code)

	_ = b.Close()
	failure := &SentError{Err: errors.New(reason), TransportFailure: transportFailure, Reason: code}
	var closeErr *websocket.CloseError
	if len(cause) > 0 && errors.As(cause[0], &closeErr) {
		failure.CloseCode = closeErr.Code
	}
	return 0, failure
}
func (b *eventBody) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	b.readMu.Lock()
	defer b.readMu.Unlock()
	var staleFrames int
	var timeout <-chan time.Time
	for {
		b.mu.Lock()
		if len(b.buffer) > 0 && !b.closed {
			n := copy(destination, b.buffer)
			b.buffer = b.buffer[n:]
			complete := b.terminal && b.successful && len(b.buffer) == 0
			b.mu.Unlock()
			if complete {
				// A streaming formatter may stop at the terminal blank line and
				// cancel on Close, without another Read for EOF. The upstream turn
				// is already drained; Close still checks cancellation and pool policy.
				_ = b.Close()
			}
			return n, nil
		}
		closed, terminal := b.closed, b.terminal
		b.mu.Unlock()
		if closed {
			if err := b.ctx.Err(); err != nil {
				return 0, &SentError{Err: err}
			}
			return 0, io.EOF
		}
		if terminal {
			_ = b.Close()
			return 0, io.EOF
		}
		var next frame
		if b.lease.readTimeout > 0 && timeout == nil {
			timer := time.NewTimer(b.lease.readTimeout)
			defer timer.Stop()
			timeout = timer.C
		}
		next, timedOut := b.nextFrame(timeout)
		if timedOut {
			return b.failure("upstream read timeout", true)
		}

		if next.err != nil {
			var closeErr *websocket.CloseError
			if errors.As(next.err, &closeErr) {
				switch closeErr.Code {
				case 1000:
					b.pool.count("socket_close_normal")
				case 1001:
					b.pool.count("socket_close_going_away")
				case 1002:
					b.pool.count("socket_close_protocol")
				case 1003:
					b.pool.count("socket_close_unsupported")
				case 1006:
					b.pool.count("socket_close_abnormal")
				case 1008:
					b.pool.count("socket_close_policy")
				case 1009:
					b.pool.count("socket_close_too_big")
				case 1011:
					b.pool.count("socket_close_internal")
				default:
					b.pool.count("socket_close_other")
				}
			} else if errors.Is(next.err, websocket.ErrReadLimit) {
				b.pool.count("socket_frame_limit")
			}
			return b.failure("upstream connection interrupted or invalid", isTransportFailure(next.err), next.err)
		}
		if validateJSON(next.data) != nil {
			return b.failure("invalid upstream JSON event", false)
		}
		var event streamEnvelope
		if json.Unmarshal(next.data, &event) != nil || !eventTypePattern.MatchString(event.Type) {
			return b.failure("invalid upstream event type", false)
		}
		id, valid := event.ownerID()
		if !valid {
			return b.failure("conflicting upstream response IDs", false)
		}
		b.mu.Lock()
		current := b.responseID
		b.mu.Unlock()
		control := isControlEvent(event.Type)
		if b.previousID != "" && id == b.previousID {
			// Only unsolicited control frames can be skipped. A repeated
			// lifecycle/terminal is an invalid response, not a new turn.
			if control && staleFrames < maxIgnoredFrames {
				staleFrames++
				b.pool.count("stale_frame_ignored")
				continue
			}
			return b.failure("stale upstream response", false)
		}
		controlWithWork := control && frameHasTurnEvidence(next.data)
		if b.previousID != "" && (current == "" || id != current) {
			// A response.failed with its own response ID can stand alone,
			// like a direct completion. A bare error cannot establish that.
			unboundFailure := event.Type == "error" || (event.Type == "response.failed" && id == "")
			if controlWithWork || (unboundFailure && frameHasTurnEvidence(next.data)) {
				return b.failure("unattributed upstream event", false)
			}
		}
		if event.Type == "response.created" || (current == "" && event.Type == "response.in_progress" && id != "") {
			if current != "" || id == "" {
				return b.failure("unexpected upstream response.created", false)
			}
			b.mu.Lock()
			b.responseID = id
			b.mu.Unlock()
			current = id
		} else if current == "" && (event.Type == "response.completed" || event.Type == "response.done") && id != "" {
			// Some compatible Responses servers send a complete terminal object directly.
			b.mu.Lock()
			b.responseID = id
			b.mu.Unlock()
			current = id
		} else if current == "" && event.Type != "error" && event.Type != "response.failed" && !isControlEvent(event.Type) {
			return b.failure("upstream event before response.created", false)
		}
		if current != "" && id != "" && id != current {
			return b.failure("upstream response ID mismatch", false)
		}
		terminal, success := false, false
		switch event.Type {
		case "response.completed", "response.done":
			terminal = true
			if event.Response == nil || event.Response.ID == "" || event.Response.ID != current || event.Response.Status != "completed" {
				return b.failure("invalid completed response", false)
			}
			success = true
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
			terminal = true
		}
		if b.lease.onEvent != nil {
			b.lease.onEvent(next.data, controlWithWork)
		}
		if terminal && !success && b.lease.onFailure != nil {
			var tail []byte
			var uncertainTail bool
			if event.Type == "error" {
				tail, uncertainTail = b.failedTail(id)
				if uncertainTail {
					b.pool.count("unattributed_failure_tail")
				}
			}
			if preferred := b.lease.onFailure(next.data, tail, uncertainTail); len(preferred) > 0 {
				next.data = preferred
				// Failure selection preserves an authoritative policy refusal when
				// a later envelope supersedes the original error.
				var selected struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(preferred, &selected) == nil && eventTypePattern.MatchString(selected.Type) {
					event.Type = selected.Type
				}
			}
		}
		var encoded bytes.Buffer
		_, _ = encoded.WriteString("event: ")
		_, _ = encoded.WriteString(event.Type)
		_ = encoded.WriteByte('\n')
		_, _ = encoded.WriteString("data: ")
		// Preserve the complete JSON document and numeric precision while
		// keeping downstream line-oriented adapters compatible with pretty JSON.
		if err := json.Compact(&encoded, next.data); err != nil {
			return b.failure("invalid upstream JSON event", false)
		}
		_, _ = encoded.WriteString("\n\n")
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			if err := b.ctx.Err(); err != nil {
				return 0, &SentError{Err: err}
			}
			return 0, &SentError{Err: errors.New("stream closed during delivery")}
		}
		b.buffer = encoded.Bytes()
		b.terminal = terminal
		b.successful = success
		b.mu.Unlock()
		if terminal {
			if success {
				b.pool.count("completed")
			} else {
				b.pool.count("terminal_error")
			}
		}
	}
}

type streamEnvelope struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id"`
	Response   *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"response"`
}

// A top-level event id is not a response id.
func (e streamEnvelope) ownerID() (string, bool) {
	if e.Response != nil && e.Response.ID != "" {
		return e.Response.ID, e.ResponseID == "" || e.ResponseID == e.Response.ID
	}
	return e.ResponseID, true
}

// Connection metadata (headers, rate limits, timestamps) is not generation or
// usage. Only inspect control frames and evidence of a possibly unrelated failure.
func frameHasTurnEvidence(data []byte) bool {
	var work bool
	inspect := func(k, v gjson.Result) bool {
		if v.Type == gjson.Null {
			return true
		}
		switch k.Str {
		case "output":
			work = !v.IsArray() || v.Get("#").Int() > 0
		case "usage", "item", "part", "content", "delta", "encrypted_content", "tool_calls", "call_id", "arguments", "output_text", "refusal", "result":
			work = true
		}
		return !work
	}
	frame := gjson.ParseBytes(data)
	frame.ForEach(inspect)
	if !work {
		frame.Get("response").ForEach(inspect)
	}
	return work
}

// Codex sends these connection/request control envelopes before response.created.
// Preserve them as SSE (including metadata headers) without treating them as
// generation ownership or copying their fields into mutable HTTP headers.
func isControlEvent(kind string) bool {
	switch kind {
	case "codex.rate_limits", "codex.response.metadata", "responsesapi.websocket_timing", "keepalive":
		return true
	default:
		return false
	}
}

const maxIgnoredFrames = 8

// Failure tails are accounting evidence only; never replay generation or lease
// the socket again. Only the same turn's bounded failed terminal is accepted.
func (b *eventBody) failedTail(id string) (lastError []byte, uncertain bool) {
	if id == "" {
		b.mu.Lock()
		id = b.responseID
		b.mu.Unlock()
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for i := 0; i < maxIgnoredFrames; i++ {
		next, timeout := b.nextFrame(timer.C)
		if timeout || next.err != nil {
			return lastError, false
		}
		var e streamEnvelope
		if validateJSON(next.data) != nil || json.Unmarshal(next.data, &e) != nil || !eventTypePattern.MatchString(e.Type) {
			return lastError, b.previousID != ""
		}
		owner, valid := e.ownerID()
		if !valid {
			return lastError, b.previousID != ""
		}
		if b.previousID != "" && owner == b.previousID {
			b.pool.count("stale_frame_ignored")
			continue
		}
		if id != "" && owner != "" && id != owner {
			return lastError, b.previousID != ""
		}
		control := isControlEvent(e.Type)
		controlWithWork := control && frameHasTurnEvidence(next.data)
		if b.previousID != "" && (id == "" || owner != id) && (controlWithWork || (!control && frameHasTurnEvidence(next.data))) {
			// Do not attribute token counts or expose the payload, but the
			// original rejection can no longer prove that replay is safe.
			return lastError, true
		}
		// Observe every same-turn frame before deciding whether a bare error is
		// retryable. A policy refusal or acceptance can precede response.failed.
		if b.lease.onEvent != nil {
			b.lease.onEvent(next.data, controlWithWork)
		}
		if e.Type == "response.failed" {
			return next.data, false
		}
		if e.Type == "error" {
			lastError = next.data
		}
		switch e.Type {
		case "response.created", "response.completed", "response.done", "response.incomplete", "response.cancelled", "response.canceled":
			return lastError, false
		}
	}
	return lastError, b.previousID != ""
}
func (b *eventBody) nextFrame(timeout <-chan time.Time) (frame, bool) {
	select {
	case f := <-b.lease.events:
		return f, false
	case <-b.ctx.Done():
		return frame{err: b.ctx.Err()}, false
	case <-b.lease.done:
		return frame{err: io.ErrClosedPipe}, false
	case <-timeout:
		return frame{}, true
	}
}
