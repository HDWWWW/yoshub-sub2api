//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsWebSocketFailuresEnqueueBeforeConnectionCloses(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 8)
	gin.SetMode(gin.TestMode)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	ready := make(chan int, 3)
	finished := make(chan struct{})
	router := gin.New()
	router.Use(func(c *gin.Context) { defer close(finished); c.Next() })
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.GET("/v1/responses", func(c *gin.Context) {
		conn, err := coderws.Accept(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
		for turn := 1; turn <= 3; turn++ {
			if _, _, err := conn.Read(c.Request.Context()); err != nil {
				return
			}
			// The real first generation may have no BeginOpsStreamTurn call.
			if turn > 1 {
				service.BeginOpsStreamTurn(c, turn)
			}
			setOpsRequestContext(c, fmt.Sprintf("client-%d", turn), true)
			service.SetOpsUpstreamModel(c, fmt.Sprintf("upstream-%d", turn))
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.AccountID, int64(200+turn)))
			groupID := int64(300 + turn)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: int64(100 + turn), GroupID: &groupID,
				User: &service.User{ID: int64(400 + turn)}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}})
			if turn <= 2 {
				message := fmt.Sprintf("turn %d rejected", turn)
				if err := conn.Write(c.Request.Context(), coderws.MessageText, []byte(`{"type":"error"}`)); err != nil {
					return
				}
				service.SetOpsUpstreamError(c, 400+turn, message, "")
				service.MarkOpsStreamFailure(c, "upstream_error", "turn_rejected", message, 400+turn)
				service.MarkOpsStreamFailure(c, "upstream_error", "generic_duplicate", "must not overwrite root cause", 502)
			} else {
				if err := conn.Write(c.Request.Context(), coderws.MessageText, []byte(`{"type":"response.completed"}`)); err != nil {
					return
				}
			}
			ready <- turn
		}
		_, _, _ = conn.Read(c.Request.Context())
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	for turn := 1; turn <= 3; turn++ {
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
		_, _, err := conn.Read(ctx)
		require.NoError(t, err)
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("handler did not finish this turn")
		}
		if turn <= 2 {
			require.Len(t, opsErrorLogQueue, 1, "the connection is still open; this failure must already be queued")
			job := <-opsErrorLogQueue
			require.Equal(t, fmt.Sprintf("turn %d rejected", turn), job.entry.ErrorMessage)
			require.Equal(t, 400+turn, job.entry.StatusCode)
			require.Equal(t, fmt.Sprintf("client-%d", turn), job.entry.Model)
			require.Equal(t, fmt.Sprintf("upstream-%d", turn), job.entry.UpstreamModel)
			require.EqualValues(t, 200+turn, *job.entry.AccountID)
			require.EqualValues(t, 300+turn, *job.entry.GroupID)
			require.EqualValues(t, 100+turn, *job.entry.APIKeyID)
		} else {
			require.Empty(t, opsErrorLogQueue, "a later success cannot repeat an earlier failure")
		}
	}
	require.NoError(t, conn.CloseNow())
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("connection handler did not exit")
	}
	require.Empty(t, opsErrorLogQueue, "closing the session must not requeue completed failures")
	require.EqualValues(t, 2, OpsErrorLogEnqueuedTotal())
}

func TestOpsWebSocketImmediateLogSurvivesTurnRestartAndHistoryBound(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 80)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.GET("/v1/responses", func(c *gin.Context) {
		service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
		c.Status(http.StatusSwitchingProtocols)
		for i := 0; i < 70; i++ {
			service.BeginOpsStreamTurn(c, 1) // a new proxy invocation can restart numbering
			service.MarkOpsStreamFailure(c, "upstream_error", "failure", fmt.Sprintf("failure %d", i), 502)
			service.MarkOpsStreamFailure(c, "upstream_error", "duplicate", "duplicate", 502)
			require.Len(t, opsErrorLogQueue, i+1)
		}
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	require.Len(t, opsErrorLogQueue, 70)
	require.EqualValues(t, 70, OpsErrorLogEnqueuedTotal())
}

func TestOpsWebSocketImmediateLogUnknownDispatchAndCancellation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown_dispatch=%t", failed), func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.GET("/v1/responses", func(c *gin.Context) {
				service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
				c.Status(http.StatusSwitchingProtocols)
				if failed {
					service.MarkOpsStreamFailure(c, "upstream_error", "smart_ws_dispatch_uncertain", "request may have executed and was not replayed", 502)
					require.Len(t, opsErrorLogQueue, 1)
				}
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			if failed {
				require.Len(t, opsErrorLogQueue, 1)
				job := <-opsErrorLogQueue
				require.Contains(t, job.entry.ErrorBody, "smart_ws_dispatch_uncertain")
				require.Equal(t, 502, job.entry.StatusCode)
			} else {
				require.Empty(t, opsErrorLogQueue)
			}
		})
	}
}

func TestOpsWebSocketImmediateFiltersStayWithinTheirTurn(t *testing.T) {
	for _, skipped := range []string{"skip_monitoring", service.OpenAIPolicyCyber, service.OpenAIPolicyBio, strings.ToUpper(service.OpenAIPolicyBio)} {
		t.Run(skipped, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.GET("/v1/responses", func(c *gin.Context) {
				service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
				c.Status(http.StatusSwitchingProtocols)
				service.BeginOpsStreamTurn(c, 1)
				if skipped == "skip_monitoring" {
					c.Set(service.OpsSkipPassthroughKey, true)
				}
				// The dedicated policy writer can clear the context mark before
				// the successful WS write callback; the actual code still owns it.
				service.MarkOpsStreamFailure(c, "invalid_request_error", skipped, "already handled by its owner", 400)
				require.Empty(t, opsErrorLogQueue)
				service.BeginOpsStreamTurn(c, 2)
				service.MarkOpsStreamFailure(c, "upstream_error", "server_error", "next turn failed", 502)
				require.Len(t, opsErrorLogQueue, 1)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			require.Len(t, opsErrorLogQueue, 1)
			job := <-opsErrorLogQueue
			require.Equal(t, "next turn failed", job.entry.ErrorMessage)
		})
	}
}
