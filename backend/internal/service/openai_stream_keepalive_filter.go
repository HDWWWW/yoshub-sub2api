package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Only a complete, valid keepalive frame may bypass first-output staging.
// In particular, the JSON type wins over an incorrect SSE event header.
type openAIStreamKeepaliveFilter struct {
	lines       []string
	bytes       int
	passthrough bool
}

func (f *openAIStreamKeepaliveFilter) Push(line string) (ordinary []string, frame string) {
	if f.passthrough {
		f.passthrough = line != ""
		return []string{line}, ""
	}
	if len(f.lines) == 0 {
		candidate := false
		if kind, ok := extractOpenAISSEEventLine(line); ok {
			candidate = strings.TrimSpace(kind) == "keepalive"
		} else if data, ok := extractOpenAISSEDataLine(line); ok {
			candidate = gjson.Valid(data) && gjson.Get(data, "type").String() == "keepalive"
		}
		if !candidate {
			// A data-only candidate cannot detach from an earlier event/id/
			// comment line that has already entered the ordinary stream parser.
			f.passthrough = line != ""
			return []string{line}, ""
		}
	}
	f.lines = append(f.lines, line)
	f.bytes += len(line) + 1
	if f.bytes > openAIEarlyControlMaxBytes {
		ordinary = f.lines
		f.lines, f.bytes, f.passthrough = nil, 0, line != ""
		return ordinary, ""
	}
	if line != "" {
		return nil, ""
	}
	lines := f.lines
	f.lines, f.bytes = nil, 0
	var parser openAICompatSSEFrameParser
	for _, part := range lines {
		if parsed, ok := parser.AddLine(part); ok {
			data := []byte(parsed.Data)
			if gjson.ValidBytes(data) && effectiveOpenAISSEEventType(data, parsed.EventType) == "keepalive" &&
				!smartWSPayloadHasWork(data) && !gjson.GetBytes(data, "delta").Exists() && !gjson.GetBytes(data, "item").Exists() {
				return nil, strings.Join(lines, "\n") + "\n"
			}
			break
		}
	}
	return lines, ""
}

func (f *openAIStreamKeepaliveFilter) Finish() (ordinary []string, frame string) {
	if len(f.lines) == 0 {
		return nil, ""
	}
	return f.Push("")
}

// Passthrough parses and writes on one goroutine. Native forwarding uses Push
// in its event loop instead, because its scanner may run on a reader goroutine.
type openAIStreamKeepaliveScanner struct {
	source interface {
		Scan() bool
		Text() string
		Err() error
	}
	filter  openAIStreamKeepaliveFilter
	enabled func() bool
	emit    func(string) error
	queue   []string
	text    string
	done    bool
	err     error
}

func (s *openAIStreamKeepaliveScanner) Scan() bool {
	for len(s.queue) == 0 && !s.done {
		var frame string
		if !s.source.Scan() {
			s.done = true
			s.queue, frame = s.filter.Finish()
		} else if s.enabled() || len(s.filter.lines) > 0 {
			s.queue, frame = s.filter.Push(s.source.Text())
		} else {
			s.queue = []string{s.source.Text()}
		}
		if frame != "" {
			if err := s.emit(frame); err != nil {
				s.err, s.done = err, true
				return false
			}
		}
	}
	if len(s.queue) == 0 {
		return false
	}
	s.text, s.queue = s.queue[0], s.queue[1:]
	return true
}

func (s *openAIStreamKeepaliveScanner) Text() string { return s.text }
func (s *openAIStreamKeepaliveScanner) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.source.Err()
}
