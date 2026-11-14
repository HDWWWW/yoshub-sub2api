package service

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Keep an outstanding socket read across a short rejection-tail timeout. The
// session context and FrameConn.Close still terminate the actual upstream read.
type smartWSNativeReadBuffer struct {
	openaiwsv2.FrameConn
	ctx     context.Context
	pending chan smartWSNativeReadResult
	closed  bool // owned by the sole relay reader
}

type smartWSNativeReadResult struct {
	typ     coderws.MessageType
	payload []byte
	err     error
}

func (b *smartWSNativeReadBuffer) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if b.pending == nil {
		b.pending = make(chan smartWSNativeReadResult, 1)
		pending := b.pending
		go func() {
			typ, payload, err := b.FrameConn.ReadFrame(b.ctx)
			pending <- smartWSNativeReadResult{typ, payload, err}
		}()
	}
	select {
	case result := <-b.pending:
		b.pending = nil
		b.closed = result.err != nil
		return result.typ, result.payload, result.err
	case <-ctx.Done():
		return coderws.MessageText, nil, ctx.Err()
	}
}

// Observes each serial native turn at the actual write boundary. Delivery to
// the client, not upstream completion alone, determines the final outcome.
type smartWSNativeObserver struct {
	openaiwsv2.FrameConn
	s         *OpenAIGatewayService
	c         *gin.Context
	account   *Account
	riskModel *upstreamResponseModelObserver
	dispatch  *smartWSDispatch
	mu        sync.Mutex
	writeMu   sync.Mutex // serialize client writes and internal retries
	current   *smartWSObservation
	turns     int
	openedAt  time.Time
	pending   []smartWSNativeFrame // owned by the sole relay reader
	payload   []byte
	retry     *smartWSNativeRetryBudget
}

type smartWSNativeFrame struct {
	typ     coderws.MessageType
	payload []byte
}

func (w *smartWSNativeObserver) WriteFrame(ctx context.Context, typ coderws.MessageType, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if gjson.GetBytes(payload, "type").String() == "response.create" {
		w.mu.Lock()
		if w.openedAt.IsZero() {
			w.openedAt = time.Now()
		}
		if w.turns > 0 && time.Since(w.openedAt) >= w.s.getOpenAIWSConnPool().maxConnAge() {
			w.mu.Unlock()
			return NewOpenAIWSClientCloseError(coderws.StatusGoingAway,
				"upstream connection reached maximum lifetime; new turn was not dispatched", nil)
		}
		w.current = w.s.beginSmartWSObservation(nil)
		sentModel := gjson.GetBytes(payload, "model").String()
		if sentModel == "" && w.c != nil {
			// Follow-up frames may inherit the session model. The policy adapter
			// records the effective model before dispatching each accepted turn.
			sentModel = w.c.GetString(OpsUpstreamModelKey)
		}
		w.riskModel = w.s.newAccountRiskModelObserver(ctx, w.c, w.account, sentModel)
		w.current.reused = w.turns > 0
		w.current.dispatched = true
		w.turns++
		w.payload = append([]byte(nil), payload...)
		w.retry = &smartWSNativeRetryBudget{}
		if w.c != nil {
			w.c.Set(smartWSFailureDeliveredKey, false)
		}
		w.dispatch.state.Store(smartWSMayBeSent)
		w.dispatch.accepted.Store(false)
		w.dispatch.policy.Store(nil)
		w.mu.Unlock()
	} else if gjson.GetBytes(payload, "type").String() == "response.cancel" {
		w.dispatch.accepted.Store(true)
	}
	return w.FrameConn.WriteFrame(ctx, typ, payload)
}
func (w *smartWSNativeObserver) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	var typ coderws.MessageType
	var payload []byte
	var err error
	if len(w.pending) > 0 {
		typ, payload = w.pending[0].typ, w.pending[0].payload
		w.pending = w.pending[1:]
	} else {
		typ, payload, err = w.FrameConn.ReadFrame(ctx)
	}
	if err == nil {
		w.mu.Lock()
		w.current.event(payload)
		riskModel := w.riskModel
		turn, currentPayload, retryBudget := w.turns, w.payload, w.retry
		if w.current != nil {
			w.dispatch.state.Store(smartWSReceived)
			w.dispatch.observe(w.c, payload)
		}
		w.mu.Unlock()
		if riskModel != nil {
			riskModel.ObserveOpenAI(payload, gjson.GetBytes(payload, "type").String())
		}
		// An immediate bare rejection can still be followed by authoritative
		// failed usage. Inspect a bounded tail before allowing account failover,
		// retaining every received frame for the normal relay/accounting path.
		if gjson.GetBytes(payload, "type").String() == "error" && (smartWSIsExplicitRateLimit(payload) || smartWSIsExplicitTemporaryRejection(payload)) {
			payload = w.readRejectionTail(ctx, payload)
		}
		payload = openAIPolicyRejectionPayload(payload)
		buffer, buffered := w.FrameConn.(*smartWSNativeReadBuffer)
		if turn > 1 && retryBudget != nil && retryBudget.attempts < w.account.GetPoolModeRetryCount() && buffered && !buffer.closed && !w.dispatch.accepted.Load() {
			failure := w.s.smartWSRetryableRejection(w.c, w.account, openAIHTTPWSEventErrorStatus(payload), nil, payload, false, gjson.GetBytes(currentPayload, "model").String())
			if failure != nil && retryBudget.wait(ctx, w.account, failure, turn) {
				payload = w.readRejectionTail(ctx, payload)
				w.writeMu.Lock()
				w.mu.Lock()
				canRetry := w.retry == retryBudget && w.current != nil && !w.dispatch.accepted.Load() && !buffer.closed && ctx.Err() == nil
				if canRetry {
					w.current.finish(ctx, nil, failure)
					w.current = w.s.beginSmartWSObservation(nil)
					w.current.reused, w.current.dispatched = true, true
					w.dispatch.state.Store(smartWSMayBeSent)
					w.mu.Unlock()
					w.pending = nil // only unaccepted, non-usage metadata can remain
					writeCtx, cancel := context.WithTimeout(ctx, w.s.openAIWSWriteTimeout())
					writeErr := w.FrameConn.WriteFrame(writeCtx, coderws.MessageText, currentPayload)
					cancel()
					w.writeMu.Unlock()
					if writeErr != nil {
						return coderws.MessageText, nil, writeErr
					}
					return w.ReadFrame(ctx)
				}
				w.mu.Unlock()
				w.writeMu.Unlock()
			}
		}
	}
	return typ, payload, err
}
func (w *smartWSNativeObserver) delivered(ctx context.Context, payload []byte, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil {
		return
	}
	terminal := normalizeOpenAIWSTerminalEvent(gjson.GetBytes(payload, "type").String())
	if terminal != "" || gjson.GetBytes(payload, "type").String() == "error" {
		w.dispatch.state.Store(smartWSTerminal)
		if err == nil && (terminal == "response.failed" || terminal == "" || terminal == "response.cancelled") {
			err = errors.New("upstream terminal error")
		}
		w.current.finish(ctx, &OpenAIForwardResult{}, err)
		w.current = nil
		w.payload = nil
	}
}
func (w *smartWSNativeObserver) finish(ctx context.Context, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		if err == nil {
			err = errors.New("upstream stream ended before a terminal event")
		}
		w.current.finish(ctx, nil, err)
	}
}

func (w *smartWSNativeObserver) readRejectionTail(ctx context.Context, payload []byte) []byte {
	tailCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	for i := 0; i < 8; i++ {
		tailType, tail, tailErr := w.FrameConn.ReadFrame(tailCtx)
		if tailErr != nil {
			break
		}
		w.pending = append(w.pending, smartWSNativeFrame{tailType, tail})
		w.dispatch.observe(w.c, tail)
		if isOpenAIContentPolicyRejection(tail) || smartWSPayloadHasWork(tail) || isOpenAIWSTerminalEvent(gjson.GetBytes(tail, "type").String()) {
			break
		}
	}
	cancel()
	preferred := w.dispatch.failurePayload(payload)
	if !bytes.Equal(preferred, payload) {
		payload = preferred
		// The selected refusal is delivered now; don't deliver it twice.
		for i, frame := range w.pending {
			if bytes.Equal(openAIPolicyRejectionPayload(frame.payload), preferred) {
				w.pending = append(w.pending[:i], w.pending[i+1:]...)
				break
			}
		}
	}
	return payload
}
