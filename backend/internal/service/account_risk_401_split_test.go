//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountRisk401LegacyAndExplicitOff(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		stop, alert bool
	}{
		{`{}`, false, false}, {`{"probe_401_enabled":true}`, true, true}, {`{"probe_401_enabled":true,"notify_401_enabled":false}`, true, false}, {`{"notify_401_enabled":true}`, false, true},
	} {
		var config AccountRiskConfig
		require.NoError(t, json.Unmarshal([]byte(tc.raw), &config))
		require.Equal(t, tc.stop, config.Probe401Enabled)
		require.Equal(t, tc.alert, config.Notify401Enabled)
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		var again AccountRiskConfig
		require.NoError(t, json.Unmarshal(raw, &again))
		require.Equal(t, config, again)
	}
}

func TestAccountRisk401IndependentSwitches(t *testing.T) {
	for _, stop := range []bool{false, true} {
		for _, alert := range []bool{false, true} {
			t.Run(fmt.Sprintf("stop=%t/alert=%t", stop, alert), func(t *testing.T) {
				s, store, accounts, _, now := riskHarness()
				r := store.state.Rules[7]
				r.Config.Probe401Enabled, r.Config.Notify401Enabled = stop, alert
				r.Config.NotifyEnabled = false
				accounts.pool[0].Credentials = map[string]any{"access_token": "fixture-token"}
				job := riskDueJob(t, s, accounts, now)
				s.probe = &AccountTestService{httpUpstream: &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(401, `{"error":{"code":"token_revoked","message":"do-not-alert"}}`)}}, accountRepo: &openAIAccountTestRepo{}}
				n := &riskNoticeStub{}
				s.notifier = n
				s.runProbe(context.Background(), job)
				require.Equal(t, !stop, accounts.pool[0].Schedulable)
				if stop || alert {
					require.Len(t, r.Events, 1)
					require.Equal(t, map[bool]string{true: "stop", false: "notify_only"}[stop], r.Events[0].Action)
				} else {
					require.Empty(t, r.Events)
				}
				require.NoError(t, s.deliverNotification(context.Background(), now))
				require.Len(t, n.messages, map[bool]int{true: 1, false: 0}[alert])
				if alert {
					require.NotContains(t, n.messages[0], "do-not-alert")
				}
				// The first alert survives the stop. Later responses from this account are silent.
				gateway := &OpenAIGatewayService{accountRisk: s}
				ctx := WithHeaderOverrideRequestScope(context.Background(), 1, 1, &r.GroupID)
				gateway.observeBusiness401(ctx, &accounts.pool[0], 401, "HTTP")
				require.NoError(t, s.deliverNotification(ctx, now.Add(time.Minute)))
				if alert && stop {
					require.Len(t, n.messages, 1)
				} else if alert {
					require.Len(t, n.messages, 2)
					require.Contains(t, n.messages[1], "业务请求")
					require.Contains(t, n.messages[1], "网关")
				} else {
					require.Empty(t, n.messages)
				}
			})
		}
	}
}

func TestAccountRisk401NotificationOnlyForNonCredentialProbe(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.Probe401Enabled = true
	r.Config.Notify401Enabled = true
	accounts.pool[0].Credentials = map[string]any{"access_token": "fixture-token"}
	job := riskDueJob(t, s, accounts, now)
	s.probe = &AccountTestService{httpUpstream: &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(401, strings.Repeat("x", 70<<10))}}, accountRepo: &openAIAccountTestRepo{}}
	s.runProbe(context.Background(), job)
	require.Len(t, r.Events, 1)
	require.Equal(t, "notify_only", r.Events[0].Action)
	require.True(t, accounts.pool[0].Schedulable)
	r.Config.Notify401Enabled = false
	s.notifier = &riskNoticeStub{}
	require.NoError(t, s.deliverNotification(context.Background(), now))
	require.Equal(t, "cancelled", r.Events[0].Notification)
}

func TestAccountRisk401SmartWSWireObservation(t *testing.T) {
	for _, kind := range []string{"handshake", "HTTP fallback", "first frame", "error and terminal"} {
		t.Run(kind, func(t *testing.T) {
			gateway, account, httpTransport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				if kind == "handshake" {
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":{"message":"private fixture"}}`)
					return
				}
				if kind == "HTTP fallback" {
					w.WriteHeader(426)
					return
				}
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				if kind == "error and terminal" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":401,"error":{"message":"private fixture"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"error":{"status_code":401,"message":"private fixture"}}}`))
				_, _, _ = conn.ReadMessage()
			})
			risk, store, accounts, _, _ := riskHarness()
			account.Status, account.Schedulable = StatusActive, true
			accounts.pool = append(accounts.pool, *account)
			rule := store.state.Rules[7]
			rule.Config.Notify401Enabled = true
			gateway.accountRisk = risk
			httpTransport.responses = []*http.Response{newJSONResponse(401, `{"detail":"private fixture"}`)}
			c, _ := forcedWSContext(t, "/v1/responses", 9, "notice")
			_, err := gateway.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"hi"}`))
			require.Error(t, err)
			require.Len(t, rule.Events, 1)
			require.Equal(t, accountRiskBusiness401, rule.Events[0].Kind)
			require.Equal(t, account.ID, rule.Events[0].AccountID)
			require.NotContains(t, rule.Events[0].Reason, "private fixture")
		})
	}
}

func TestSmartWSOfficialClientErrorWire(t *testing.T) {
	for _, code := range []string{"slow_down", "invalid_encrypted_content", "context_length_exceeded"} {
		t.Run(code, func(t *testing.T) {
			a := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "private-test-key", "base_url": "https://relay.invalid/v1"}}
			body := []byte(`{"type":"response.failed","response":{"id":"resp_x","usage":{"input_tokens":7},"error":{"type":"invalid_request_error","code":"` + code + `","param":"input[0]","message":"Ⱥ RELAY.INVALID private-test-key","internal":"secret-extra"}}}`)
			result := smartWSClientFrame(nil, a, body)
			expected := code
			if code == "slow_down" {
				expected = "server_error"
			}
			require.Equal(t, expected, gjson.GetBytes(result, "response.error.code").Str)
			require.Equal(t, "input[0]", gjson.GetBytes(result, "response.error.param").Str)
			require.EqualValues(t, 7, gjson.GetBytes(result, "response.usage.input_tokens").Int())
			require.NotContains(t, string(result), "private-test-key")
			require.NotContains(t, string(result), "secret-extra")
			require.NotContains(t, string(result), "RELAY.INVALID")
			require.Contains(t, string(body), code)
		})
	}
}

func TestSmartWSHeaderOverridesOnHandshakeAndFallback(t *testing.T) {
	handshakeHeaders := make(chan http.Header, 1)
	gateway, a, up := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		handshakeHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusUpgradeRequired)
	})
	a.Credentials[credKeyHeaderOverrideEnabled] = true
	a.Credentials[credKeyHeaderOverrides] = map[string]any{"x-reported-key": "{{api_key_id}}", "x-reported-request": "{{request_id}}"}
	up.responses = []*http.Response{newJSONResponse(400, `{"error":{"message":"fixture"}}`)}
	c, _ := forcedWSContext(t, "/v1/responses", 17, "header-scope")
	group := int64(7)
	ctx := WithHeaderOverrideRequestScope(context.WithValue(c.Request.Context(), ctxkey.RequestID, "local-req"), 3, 17, &group)
	c.Request = c.Request.WithContext(ctx)
	_, err := gateway.Forward(ctx, c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hi"}`))
	require.Error(t, err)
	var handshake http.Header
	select {
	case handshake = <-handshakeHeaders:
	default:
		t.Fatal("SmartWS did not send the upgrade request")
	}
	require.Equal(t, "17", handshake.Get("X-Reported-Key"))
	require.Empty(t, handshake.Get("X-Reported-Request"))
	require.Equal(t, 1, up.callCount)
	require.Equal(t, "local-req", headerOverrideReportedHeaders(ctx, a.ID)["x-reported-request"])
}
