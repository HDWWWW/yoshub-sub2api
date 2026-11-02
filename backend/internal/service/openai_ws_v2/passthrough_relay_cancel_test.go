package openai_ws_v2

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestRelayCancelOnClientDisconnectStopsWithoutDrainAndPreservesCurrentUsage(t *testing.T) {
	for _, closeErr := range []error{io.EOF, io.ErrUnexpectedEOF, coderws.CloseError{Code: coderws.StatusAbnormalClosure}, &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}} {
		t.Run(closeErr.Error(), func(t *testing.T) {
			clientBase := newPassthroughTestFrameConn(nil, false)
			client := &eofReplacementFrameConn{FrameConn: clientBase, err: closeErr}
			upstream := newPassthroughTestFrameConn([]passthroughTestFrame{{msgType: coderws.MessageText, payload: []byte(`{"type":"response.in_progress","response":{"id":"resp_partial","model":"gpt-5.1","usage":{"input_tokens":9,"output_tokens":4}}}`)}}, false)
			observed := make(chan struct{})
			var once sync.Once
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			type outcome struct {
				result RelayResult
				exit   *RelayExit
			}
			done := make(chan outcome, 1)
			go func() {
				result, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"gpt-5.1"}`), RelayOptions{CancelOnClientDisconnect: true, UpstreamDrainTimeout: time.Hour, AfterClientWrite: func(_ coderws.MessageType, _ []byte, err error) {
					if err == nil {
						once.Do(func() { close(observed) })
					}
				}})
				done <- outcome{result, exit}
			}()
			select {
			case <-observed:
			case <-ctx.Done():
				t.Fatal("usage frame not read")
			}
			require.NoError(t, client.Close())
			select {
			case got := <-done:
				require.NotNil(t, got.exit)
				require.ErrorIs(t, got.exit.Err, context.Canceled)
				require.Equal(t, "client_disconnected", got.exit.Stage)
				require.NotNil(t, got.result.UnfinishedTurn)
				require.Equal(t, Usage{InputTokens: 9, OutputTokens: 4}, got.result.UnfinishedTurn.Usage)
				require.Equal(t, "resp_partial", got.result.UnfinishedTurn.RequestID)
				require.Zero(t, got.result.DroppedDownstreamFrames)
			case <-time.After(400 * time.Millisecond):
				t.Fatal("client disconnect waited for upstream drain")
			}
		})
	}
}
