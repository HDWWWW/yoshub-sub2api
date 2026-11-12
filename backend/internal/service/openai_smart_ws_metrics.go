package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const smartWSObservationKey = "openai_smart_ws_observation"

// Process-local, bounded samples. No account, prompt or credential is retained.
type smartWSMetrics struct {
	handshake401, handshake403, handshakeRejection, rejectionBypass, continuationUnavailable atomic.Int64
	nativeConnections, incomplete, backgroundAccepted                                        atomic.Int64
	attempted, succeeded, failed, canceled                                                   atomic.Int64
	handshakes, handshakeFailed, handshakeAuth, handshakeRateLimited, handshakeUnsupported   atomic.Int64
	queueRejected, capacityRejected, fallback, upstreamError, transportError, uncertain      atomic.Int64
	mu                                                                                       sync.Mutex
	samples                                                                                  [256]smartWSSample
	next, count                                                                              int
}
type smartWSSample struct {
	reused         bool
	ttft, duration int64
}
type SmartWSLatency struct {
	Samples       int   `json:"samples"`
	TTFTSamples   int   `json:"ttft_samples"`
	TTFTP50MS     int64 `json:"ttft_p50_ms"`
	TTFTP95MS     int64 `json:"ttft_p95_ms"`
	DurationP95MS int64 `json:"duration_p95_ms"`
}
type SmartWSSnapshot struct {
	Scope       string           `json:"scope"`
	SmartPool   map[string]any   `json:"smart_pool,omitempty"`
	StartedAt   time.Time        `json:"started_at"`
	Connections int              `json:"connections"`
	Acquiring   int              `json:"acquiring"`
	Handshaking int              `json:"handshaking"`
	Active      int              `json:"active"`
	Counters    map[string]int64 `json:"counters"`
	Limits      map[string]int   `json:"limits"`
	Cold        SmartWSLatency   `json:"cold"`
	Reused      SmartWSLatency   `json:"reused"`
}
type smartWSObservation struct {
	mu            sync.Mutex
	m             *smartWSMetrics
	start         time.Time
	once          sync.Once
	reused        bool
	firstMS       int64
	terminal      string
	dispatched    bool
	fallback      bool
	errorObserved bool
}

func (s *OpenAIGatewayService) beginSmartWSObservation(c *gin.Context) *smartWSObservation {
	o := &smartWSObservation{m: &s.getOpenAIWSConnPool().smartMetrics, start: time.Now(), firstMS: -1}
	o.m.attempted.Add(1)
	if c != nil {
		c.Set(smartWSObservationKey, o)
	}
	return o
}

func (o *smartWSObservation) event(payload []byte) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	typ := gjson.GetBytes(payload, "type").String()
	if typ == "error" || typ == "response.failed" {
		if !o.errorObserved {
			o.m.upstreamError.Add(1)
			o.errorObserved = true
		}
	}
	if terminal := normalizeOpenAIWSTerminalEvent(typ); terminal != "" {
		o.terminal = terminal
	}
	if typ == "error" {
		o.terminal = typ
	}
	if gjson.GetBytes(payload, "response.status").String() == "failed" {
		o.terminal = "response.failed"
		if !o.errorObserved {
			o.m.upstreamError.Add(1)
			o.errorObserved = true
		}
	}
	progress := smartWSUsefulOutput(payload, typ)
	if o.firstMS < 0 && !isOpenAIWSTerminalEvent(typ) && typ != "error" && progress {
		o.firstMS = time.Since(o.start).Milliseconds()
	}
}

// Match actual content only. Metadata, pings and gateway status events are not
// model output and cannot reduce the measured first-output latency.
func smartWSUsefulOutput(payload []byte, typ string) bool {
	switch typ {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.refusal.delta",
		"response.audio.delta", "response.audio_transcript.delta", "response.output_audio.delta", "response.output_audio_transcript.delta",
		"response.code_interpreter_call_code.delta", "response.output_text.done", "response.reasoning_summary_text.done",
		"response.reasoning_text.done", "response.audio_transcript.done", "response.function_call_arguments.done",
		"response.custom_tool_call_input.done", "response.image_generation_call.partial_image",
		"response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done", "response.output_item.added", "response.output_item.done":
		return openAIStreamDataStartsVisibleOutput(string(payload), typ) ||
			(typ == "response.output_item.done" && gjson.GetBytes(payload, "item.encrypted_content").String() != "")
	}
	return false
}

func (s *OpsService) GetSmartWSMetrics(ctx context.Context) (SmartWSSnapshot, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return SmartWSSnapshot{}, err
	}
	if s.openAIGatewayService == nil {
		return SmartWSSnapshot{Scope: "process"}, nil
	}
	snapshot := s.openAIGatewayService.getOpenAIWSConnPool().SmartWSSnapshot()
	snapshot.SmartPool = s.openAIGatewayService.independentSmartWSStats()
	return snapshot, nil
}

func (o *smartWSObservation) finish(ctx context.Context, result *OpenAIForwardResult, err error) {
	if o == nil {
		return
	}
	o.once.Do(func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		// A client can close immediately after consuming the terminal frame.
		// The formatter's completed result is the delivery snapshot; a later
		// request-context cancellation must not turn that success into a cancel.
		delivered := err == nil && result != nil && !result.ClientDisconnect && !o.errorObserved &&
			(o.terminal == "response.completed" || o.terminal == "response.done")
		if !delivered && (errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) || (result != nil && result.ClientDisconnect)) {
			o.m.canceled.Add(1)
		} else if o.terminal == "background.accepted" && err == nil {
			o.m.backgroundAccepted.Add(1)
		} else if o.terminal == "response.incomplete" {
			o.m.incomplete.Add(1)
			o.m.failed.Add(1)
		} else if delivered {
			o.m.succeeded.Add(1)
			if o.dispatched && !o.fallback {
				o.m.mu.Lock()
				o.m.samples[o.m.next] = smartWSSample{o.reused, o.firstMS, time.Since(o.start).Milliseconds()}
				o.m.next = (o.m.next + 1) % len(o.m.samples)
				if o.m.count < len(o.m.samples) {
					o.m.count++
				}
				o.m.mu.Unlock()
			}
		} else {
			o.m.failed.Add(1)
		}
		if o.dispatched && o.terminal == "" && err != nil {
			o.m.uncertain.Add(1)
			if !errors.Is(err, context.Canceled) {
				o.m.transportError.Add(1)
			}
		}
	})
}

func finishSmartWSObservation(c *gin.Context, result *OpenAIForwardResult, err error) {
	if c == nil {
		return
	}
	v, _ := c.Get(smartWSObservationKey)
	o, _ := v.(*smartWSObservation)
	o.finish(c.Request.Context(), result, err)
}

func (p *openAIWSConnPool) SmartWSSnapshot() SmartWSSnapshot {
	m := &p.smartMetrics
	s := SmartWSSnapshot{Scope: "process", StartedAt: p.startedAt, Counters: map[string]int64{
		"handshake_401": m.handshake401.Load(), "handshake_403": m.handshake403.Load(), "handshake_rejection": m.handshakeRejection.Load(), "rejection_bypass": m.rejectionBypass.Load(), "continuation_unavailable": m.continuationUnavailable.Load(),
		"incomplete":          m.incomplete.Load(),
		"background_accepted": m.backgroundAccepted.Load(),
		"attempted":           m.attempted.Load(), "succeeded": m.succeeded.Load(), "failed": m.failed.Load(), "canceled": m.canceled.Load(),
		"handshakes": m.handshakes.Load(), "handshake_failed": m.handshakeFailed.Load(), "handshake_auth": m.handshakeAuth.Load(), "handshake_rate_limited": m.handshakeRateLimited.Load(), "handshake_unsupported": m.handshakeUnsupported.Load(),
		"queue_rejected": m.queueRejected.Load(), "capacity_rejected": m.capacityRejected.Load(), "safe_http_fallback": m.fallback.Load(), "upstream_terminal_error": m.upstreamError.Load(), "transport_error": m.transportError.Load(), "dispatch_uncertain": m.uncertain.Load(),
		"reused": p.metrics.acquireReuseTotal.Load(), "acquired": p.metrics.acquireTotal.Load(), "queued": p.metrics.acquireQueueWaitTotal.Load(), "queue_wait_ms": p.metrics.acquireQueueWaitMs.Load(),
	}}
	p.gate.mu.Lock()
	s.Connections = p.gate.conns
	s.Acquiring = p.gate.queued
	s.Handshaking = p.gate.dialing
	p.gate.mu.Unlock()
	s.Active = int(m.nativeConnections.Load())
	p.accounts.Range(func(_, value any) bool {
		ap := value.(*openAIWSAccountPool) //nolint:errcheck // Pool map is populated only with *openAIWSAccountPool.
		ap.mu.Lock()
		for _, conn := range ap.conns {
			if conn != nil && conn.isLeased() {
				s.Active++
			}
		}
		ap.mu.Unlock()
		return true
	})
	c := p.smartWSLimits()
	s.Limits = map[string]int{"connections": c.MaxConnsGlobal, "connections_per_identity": c.MaxConnsPerIdentity, "acquiring": c.MaxQueuedGlobal, "acquiring_per_identity": c.MaxQueuedPerIdentity, "handshakes": c.MaxConcurrentHandshakes, "handshakes_per_identity": c.MaxConcurrentHandshakesPerIdentity, "handshakes_per_second": c.HandshakesPerSecond, "handshakes_per_second_per_identity": c.HandshakesPerSecondPerIdentity}
	s.Limits["handshake_rejection_cooldown_seconds"] = c.HandshakeRejectionCooldownSeconds
	s.Limits["max_handshake_rejections"] = c.MaxHandshakeRejections
	m.mu.Lock()
	samples := append([]smartWSSample(nil), m.samples[:m.count]...)
	m.mu.Unlock()
	for _, reused := range []bool{false, true} {
		var ttft, duration []int64
		for _, v := range samples {
			if v.reused == reused {
				duration = append(duration, v.duration)
				if v.ttft >= 0 {
					ttft = append(ttft, v.ttft)
				}
			}
		}
		pct := func(v []int64, percent int) int64 {
			if len(v) == 0 {
				return 0
			}
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			return v[(len(v)*percent+99)/100-1]
		}
		l := SmartWSLatency{len(duration), len(ttft), pct(ttft, 50), pct(ttft, 95), pct(duration, 95)}
		if reused {
			s.Reused = l
		} else {
			s.Cold = l
		}
	}
	return s
}
