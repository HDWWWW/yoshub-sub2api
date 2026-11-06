package service

import (
	"context"
	"errors"
	"sync"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Observes each serial native turn at the actual write boundary. Delivery to
// the client, not upstream completion alone, determines the final outcome.
type smartWSNativeObserver struct {
	openaiwsv2.FrameConn
	s        *OpenAIGatewayService
	c        *gin.Context
	dispatch *smartWSDispatch
	mu       sync.Mutex
	current  *smartWSObservation
	turns    int
	openedAt time.Time
}

func (w *smartWSNativeObserver) WriteFrame(ctx context.Context, typ coderws.MessageType, payload []byte) error {
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
		w.current.reused = w.turns > 0
		w.current.dispatched = true
		w.turns++
		if w.c != nil {
			w.c.Set(smartWSFailureDeliveredKey, false)
		}
		w.dispatch.state.Store(smartWSMayBeSent)
		w.mu.Unlock()
	}
	return w.FrameConn.WriteFrame(ctx, typ, payload)
}
func (w *smartWSNativeObserver) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	typ, payload, err := w.FrameConn.ReadFrame(ctx)
	if err == nil {
		w.mu.Lock()
		w.current.event(payload)
		if w.current != nil {
			w.dispatch.state.Store(smartWSReceived)
		}
		w.mu.Unlock()
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
