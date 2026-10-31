//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const fixtureRefillWebhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fixture-only"

type refillNoticeStub struct {
	messages []string
	err      error
}

func (n *refillNoticeStub) Send(_ context.Context, _ string, message string) error {
	n.messages = append(n.messages, message)
	return n.err
}

type refillNoticeTransport func(*http.Request) (*http.Response, error)

func (f refillNoticeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefillNotificationsEncryptedAndIndependent(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	ctx := context.Background()
	c := refillTestConfig()
	c.Enabled, c.MonitorEnabled = false, false
	c.Notifications = AccountRefillNotificationConfig{LowBalance: true, LowBalanceThreshold: 100}
	require.Error(t, s.Configure(ctx, c, "", 1), "enabled notifications need a robot")
	require.NoError(t, s.Configure(ctx, c, "", 1, fixtureRefillWebhook))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, v.WebhookConfigured)
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "fixture-only")
	require.NotContains(t, string(raw), "encrypted_webhook")
	require.NoError(t, s.Configure(ctx, c, "", 2, ""), "blank preserves the robot")
	n := &refillNoticeStub{}
	s.notifier = n
	provider := &manualRefillProvider{p, 99}
	s.provider = provider
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "可用积分：99")
	require.Empty(t, p.submitted)
	for _, balance := range []float64{90, 80, 101, 99} {
		provider.available = balance
		store.mutate(t, func(v *accountRefillState) { v.NextBalanceCheckAt = time.Time{} })
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, n.messages, 2, "only alert again after recovery")
}
func TestRefillStartedNotificationDoesNotRepeatOrBlockPurchase(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			s, store, p, _ := refillHarness(t)
			n := &refillNoticeStub{err: errors.New("notification offline")}
			s.notifier = n
			c := refillTestConfig()
			c.Notifications.RefillStarted = true
			require.NoError(t, s.Configure(context.Background(), c, "", 1, fixtureRefillWebhook))
			if manual {
				s.provider = &manualRefillProvider{p, 500}
				r := manualRequest()
				r.Revision = 2
				require.NoError(t, s.Manual(context.Background(), r))
			}
			p.submitError = errors.New("uncertain provider timeout")
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, n.messages, 1)
			require.Len(t, p.submitted, 1, "notification failure must not stop purchase")
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, n.messages, 1)
			require.Len(t, p.submitted, 2)
			require.Equal(t, p.submitted[0].ID, p.submitted[1].ID)
			v, err := s.View(context.Background())
			require.NoError(t, err)
			require.Contains(t, v.NotificationStatus.Message, "失败")
		})
	}
}
func TestRefillWebhookValidationAndResponse(t *testing.T) {
	for _, value := range []string{"http://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x", "https://127.0.0.1/cgi-bin/webhook/send?key=x", fixtureRefillWebhook + "&key=y", fixtureRefillWebhook + "#x", "https://qyapi.weixin.qq.com:443/cgi-bin/webhook/send?key=x", "https://qyapi.weixin.qq.com/other?key=x"} {
		require.Error(t, validateRefillWebhook(value))
	}
	for _, body := range []string{`{"errcode":0}`, `{"errcode":93000,"errmsg":"secret must not be returned"}`, `{}`, `invalid`} {
		n := weComRefillNotifier{client: &http.Client{Transport: refillNoticeTransport(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			require.Equal(t, "text", payload["msgtype"])
			require.Equal(t, map[string]any{"content": "start"}, payload["text"])
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		err := n.Send(context.Background(), fixtureRefillWebhook, "start")
		if body == `{"errcode":0}` {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		}
	}
}
func TestRefillInsufficientBalanceAlertDoesNotRearmBeforeEnoughForPurchase(t *testing.T) {
	s, store, _, _ := refillHarness(t)
	n := &refillNoticeStub{}
	s.notifier = n
	c := refillTestConfig()
	c.Notifications = AccountRefillNotificationConfig{LowBalance: true, LowBalanceThreshold: 10}
	require.NoError(t, s.Configure(context.Background(), c, "", 1, fixtureRefillWebhook))
	for _, sample := range [][2]float64{{30, 60}, {30, 0}, {30, 60}, {70, 0}, {30, 60}} {
		require.NoError(t, store.WithLock(context.Background(), func(raw string, save func(string) error) error {
			state, err := decodeAccountRefillState(raw)
			require.NoError(t, err)
			return s.notifyRefillBalance(context.Background(), &state, sample[0], sample[1], func() error { return saveRefillState(&state, save) })
		}))
	}
	require.Len(t, n.messages, 2)
}
