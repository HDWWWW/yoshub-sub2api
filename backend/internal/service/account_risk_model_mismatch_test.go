//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountRiskModelMismatchActions(t *testing.T) {
	for _, action := range []string{"", "off", "stop", "notify_only"} {
		t.Run(action, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.ModelMismatchAction = action
			s.notifier = &riskNoticeStub{}
			require.NoError(t, s.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "gpt-6.1-sol", "gpt-5.4", now))
			if action == "" || action == "off" {
				require.Empty(t, store.stopped)
				require.Empty(t, r.Events)
				return
			}
			require.Len(t, r.Events, 1)
			require.Equal(t, "pending", r.Events[0].Notification, "model alerts do not require the capacity/probe notification switch")
			require.NoError(t, s.deliverNotification(context.Background(), now))
			n := s.notifier.(*riskNoticeStub)
			require.Len(t, n.messages, 1)
			require.Contains(t, n.messages[0], "gpt-6.1-sol")
			require.Contains(t, n.messages[0], "gpt-5.4")
			if action == "stop" {
				require.Equal(t, []int64{1}, store.stopped)
				require.Contains(t, n.messages[0], "已关闭此账号调度")
			} else {
				require.Empty(t, store.stopped)
				require.Contains(t, n.messages[0], "仅通知")
				require.NotContains(t, n.messages[0], "已关闭此账号调度")
			}
			require.Equal(t, "sent", r.Events[0].Notification)
		})
	}
}

func TestAccountRiskModelMismatchGuards(t *testing.T) {
	for _, scenario := range []string{"match", "empty response", "empty sent", "alias", "master off", "wrong group", "inactive group", "removed member", "credentials changed", "already stopped", "CAS", "database failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			store.state.Rules[7].Config.ModelMismatchAction = "stop"
			account := accounts.pool[0]
			groupID, sent, returned := int64(7), "gpt-6.1-sol", "gpt-5.4"
			switch scenario {
			case "match":
				returned = " GPT-6.1-SOL "
			case "empty response":
				returned = ""
			case "empty sent":
				sent = ""
			case "alias":
				sent, returned = "grok-4.5-latest", "grok-4.5-build"
			case "master off":
				store.state.Rules[7].Config.Enabled = false
			case "wrong group":
				groupID = 8
			case "inactive group":
				s.groups.(*riskTestGroups).inactive = true
			case "removed member":
				accounts.pool[0].GroupIDs = []int64{8}
			case "credentials changed":
				accounts.pool[0].Credentials = map[string]any{"api_key": "changed"}
			case "already stopped":
				accounts.pool[0].Schedulable = false
			case "CAS":
				store.cas = false
			case "database failure":
				store.fail = true
			}
			err := s.HandleModelMismatch(context.Background(), groupID, &account, sent, returned, now)
			if scenario == "database failure" || scenario == "CAS" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, store.stopped)
			require.Empty(t, store.state.Rules[7].Events)
		})
	}
}

func TestAccountRiskModelMismatchConcurrentNotificationsAndModeChange(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.ModelMismatchAction = "notify_only"
	s2 := &AccountRiskService{store: store, accounts: accounts, groups: s.groups}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			worker := s
			if i%2 == 0 {
				worker = s2
			}
			require.NoError(t, worker.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "gpt-6.1-sol", "gpt-5.4", now))
		}(i)
	}
	wg.Wait()
	require.Len(t, r.Events, 1)
	require.Equal(t, 20, r.Events[0].Occurrences)
	require.Empty(t, store.stopped)
	require.NoError(t, s2.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "gpt-6.1-sol", "gpt-5.4", now.Add(time.Minute)))
	require.Len(t, r.Events, 2, "ongoing anomalies can alert after the fixed minute")
	r.Config.ModelMismatchAction = "stop"
	require.NoError(t, s.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "gpt-6.1-sol", "gpt-5.4", now.Add(time.Minute+time.Second)))
	require.Equal(t, []int64{1}, store.stopped, "notify coalescing must never suppress a new stop")
}

func TestAccountRiskModelMismatchNotificationFailureDisableAndRestart(t *testing.T) {
	for _, scenario := range []string{"disabled", "master off", "failed", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.ModelMismatchAction = "stop"
			n := &riskNoticeStub{}
			s.notifier = n
			require.NoError(t, s.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "requested", "other", now))
			switch scenario {
			case "disabled":
				r.Config.ModelMismatchAction = "off"
			case "master off":
				r.Config.Enabled = false
			case "failed":
				n.err = errors.New("webhook unavailable")
			}
			require.NoError(t, s.deliverNotification(context.Background(), now))
			s2 := &AccountRiskService{store: store, groups: s.groups, notifier: n}
			require.NoError(t, s2.deliverNotification(context.Background(), now.Add(time.Minute)))
			require.Equal(t, []int64{1}, store.stopped, "notification outcome must not undo a stop")
			if scenario == "disabled" || scenario == "master off" {
				require.Empty(t, n.messages)
				require.Equal(t, "cancelled", r.Events[0].Notification)
			} else {
				require.Len(t, n.messages, 1)
			}
		})
	}
}

func TestAccountRiskModelMismatchStreamingBeforeCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmtBool(passthrough), func(t *testing.T) {
			risk, store, accounts, _, _ := riskHarness()
			store.state.Rules[7].Config.ModelMismatchAction = "stop"
			gateway := &OpenAIGatewayService{accountRisk: risk}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			groupID := int64(7)
			c.Set("api_key", &APIKey{GroupID: &groupID})
			gateway.beginAccountRiskModelObservation(context.Background(), c, &accounts.pool[0])
			SetOpsUpstreamModel(c, "mapped-model")
			reader, writer := io.Pipe()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
			done := make(chan error, 1)
			go func() {
				var err error
				if passthrough {
					_, err = gateway.handleStreamingResponsePassthrough(context.Background(), resp, c, &accounts.pool[0], time.Now(), "public-alias", "mapped-model")
				} else {
					_, err = gateway.handleStreamingResponse(context.Background(), resp, c, &accounts.pool[0], time.Now(), "public-alias", "mapped-model")
				}
				done <- err
			}()
			t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
			_, err := io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"wrong-model\"}}\n\n")
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				store.mu.Lock()
				defer store.mu.Unlock()
				return len(store.stopped) == 1
			}, 2*time.Second, time.Millisecond)
			select {
			case <-done:
				t.Fatal("response must still be in progress when stopped")
			default:
			}
			_, err = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"wrong-model\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
			require.NoError(t, err)
			_ = writer.Close()
			require.NoError(t, <-done)
			require.Len(t, store.stopped, 1, "repeated stream declarations are handled once")
		})
	}
}

func fmtBool(value bool) string {
	if value {
		return "passthrough"
	}
	return "converted"
}

func TestAccountRiskModelMismatchObserverMappingAndCancellation(t *testing.T) {
	risk, store, accounts, _, _ := riskHarness()
	store.state.Rules[7].Config.ModelMismatchAction = "stop"
	gateway := &OpenAIGatewayService{accountRisk: risk}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	groupID := int64(7)
	c.Set("api_key", &APIKey{GroupID: &groupID})
	ctx, cancel := context.WithCancel(context.Background())
	observer := gateway.beginAccountRiskModelObservation(ctx, c, &accounts.pool[0])
	observer.Observe("unknown-until-sent-model-is-known", false)
	require.Empty(t, store.stopped)
	SetOpsUpstreamModel(c, "mapped-model")
	observer.Observe("MAPPED-MODEL", false)
	require.Empty(t, store.stopped)
	SetOpsUpstreamModel(c, "fallback-model")
	observer.Observe("fallback-model", false)
	require.Empty(t, store.stopped)
	store.busy = 2
	cancel()
	observer.Observe("wrong-model", false)
	require.Equal(t, []int64{1}, store.stopped)
	require.Contains(t, store.state.Rules[7].Events[0].Reason, "fallback-model")
}

func TestAccountRiskModelMismatchLegacyConfigurationAndValidation(t *testing.T) {
	c := DefaultAccountRiskConfig()
	require.Equal(t, "off", c.ModelMismatchAction)
	for _, action := range []string{"", "off", "stop", "notify_only"} {
		c.ModelMismatchAction = action
		require.NoError(t, c.Validate())
	}
	c.ModelMismatchAction = "invalid"
	require.Error(t, c.Validate())
	var legacy AccountRiskConfig
	raw, _ := json.Marshal(DefaultAccountRiskConfig())
	require.NoError(t, json.Unmarshal([]byte(strings.Replace(string(raw), `"model_mismatch_action":"off",`, "", 1)), &legacy))
	require.NoError(t, legacy.Validate())
	require.False(t, riskModelMismatchEnabled(legacy))
}

func TestAccountRiskModelMismatchNativeWSPerTurn(t *testing.T) {
	risk, store, accounts, _, _ := riskHarness()
	store.state.Rules[7].Config.ModelMismatchAction = "notify_only"
	gateway := &OpenAIGatewayService{accountRisk: risk}
	t.Cleanup(func() { gateway.getOpenAIWSConnPool().Close() })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	groupID := int64(7)
	c.Set("api_key", &APIKey{GroupID: &groupID})
	conn := &fakePassthroughFrameConn{reads: [][]byte{
		[]byte(`{"type":"response.created","response":{"model":"other"}}`),
		[]byte(`{"type":"response.completed","response":{"model":"other"}}`),
		[]byte(`{"type":"response.created","response":{"model":"next-model"}}`),
		[]byte(`{"type":"response.completed","response":{"model":"third-model"}}`),
	}}
	w := &smartWSNativeObserver{FrameConn: conn, s: gateway, c: c, account: &accounts.pool[0], dispatch: &smartWSDispatch{}}
	ctx := context.Background()
	require.NoError(t, w.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"first-model"}`)))
	_, _, err := w.ReadFrame(ctx)
	require.NoError(t, err)
	require.Len(t, store.state.Rules[7].Events, 1, "notification is recorded before the terminal frame")
	_, payload, err := w.ReadFrame(ctx)
	require.NoError(t, err)
	w.delivered(ctx, payload, nil)
	require.Equal(t, 1, store.state.Rules[7].Events[0].Occurrences)
	SetOpsUpstreamModel(c, "next-model")
	require.NoError(t, w.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
	_, _, err = w.ReadFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, store.state.Rules[7].Events[0].Occurrences, "each turn uses its own sent model")
	_, payload, err = w.ReadFrame(ctx)
	require.NoError(t, err)
	w.delivered(ctx, payload, nil)
	require.Equal(t, 2, store.state.Rules[7].Events[0].Occurrences)
	require.Empty(t, store.stopped)
	require.Len(t, conn.writes, 2, "notify-only keeps the native connection usable")
}

func TestAccountRiskModelMismatchNewAttemptDoesNotReuseOldAccount(t *testing.T) {
	risk, store, accounts, _, _ := riskHarness()
	store.state.Rules[7].Config.ModelMismatchAction = "stop"
	gateway := &OpenAIGatewayService{accountRisk: risk}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	groupID := int64(7)
	c.Set("api_key", &APIKey{GroupID: &groupID})
	gateway.beginAccountRiskModelObservation(context.Background(), c, &accounts.pool[0])
	SetOpsUpstreamModel(c, "first-model")
	observer := gateway.beginAccountRiskModelObservation(context.Background(), c, &accounts.pool[1])
	observer.Observe("no-current-outbound-model", false)
	require.Empty(t, store.stopped)
	SetOpsUpstreamModel(c, "second-model")
	observer.Observe("other", false)
	require.Equal(t, []int64{2}, store.stopped)
}
