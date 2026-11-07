//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type riskNoticeStub struct {
	mu       sync.Mutex
	groups   []int64
	messages []string
	err      error
	after    func()
}

func (n *riskNoticeStub) RiskWebhookConfigured(context.Context, int64) (bool, error) {
	return true, nil
}
func (n *riskNoticeStub) SendRiskNotification(_ context.Context, id int64, content string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.groups = append(n.groups, id)
	n.messages = append(n.messages, content)
	if n.after != nil {
		n.after()
	}
	return n.err
}

func TestAccountRiskNotificationCapacityAndDefaultOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, store, accounts, _, now := riskHarness()
		n := &riskNoticeStub{}
		s.notifier = n
		store.state.Rules[7].Config.NotifyEnabled = enabled
		accounts.pool[0].Name = "priority account"
		var job *accountRiskJob
		for sec := 0; sec <= 10; sec++ {
			current, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
			require.NoError(t, err)
			if current != nil {
				job = current
			}
		}
		require.NotNil(t, job)
		require.Empty(t, store.stopped)
		require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
		require.Equal(t, []int64{1}, store.stopped)
		require.Empty(t, n.messages, "no network sends inside the stop transaction")
		require.NoError(t, s.deliverNotification(context.Background(), now.Add(11*time.Second)))
		if enabled {
			require.Equal(t, []int64{7}, n.groups)
			require.Len(t, n.messages, 1)
			require.Contains(t, n.messages[0], "59.0%")
			require.Contains(t, n.messages[0], "priority account")
			require.Equal(t, "sent", store.state.Rules[7].Events[0].Notification)
		} else {
			require.Empty(t, n.messages)
		}
		// Simulate restart: same durable store, independent service instance.
		s2 := &AccountRiskService{store: store, groups: s.groups, notifier: n}
		require.NoError(t, s2.deliverNotification(context.Background(), now.Add(time.Hour)))
		if enabled {
			require.Len(t, n.messages, 1)
		} else {
			require.Empty(t, n.messages)
		}
	}
}

func TestAccountRiskNotificationProbeGuards(t *testing.T) {
	for _, scenario := range []string{"mismatch", "healthy", "incomplete", "429", "cancelled", "CAS", "save failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			store.state.Rules[7].Config.NotifyEnabled = true
			job := riskDueJob(t, s, accounts, now)
			n := &riskNoticeStub{}
			s.notifier = n
			result := AccountRiskProbeResult{Completed: true, Text: "private reply without expected number"}
			var upstreamErr error
			switch scenario {
			case "healthy":
				result.Text = "21"
			case "incomplete":
				result.Completed = false
			case "429":
				upstreamErr = errors.New("HTTP 429")
			case "cancelled":
				upstreamErr = context.Canceled
			case "CAS":
				store.cas = false
			case "save failure":
				store.fail = true
			}
			err := s.finishProbe(context.Background(), job, result, upstreamErr, now.Add(301*time.Second))
			if scenario == "save failure" {
				require.Error(t, err)
				store.fail = false
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, s.deliverNotification(context.Background(), now.Add(302*time.Second)))
			if scenario == "mismatch" {
				require.Len(t, n.messages, 1)
				require.Contains(t, n.messages[0], "完整回复不含期望内容")
				require.NotContains(t, n.messages[0], result.Text)
				require.NotContains(t, n.messages[0], job.config.ProbePrompt)
			} else {
				require.Empty(t, n.messages)
			}
		})
	}
}

func TestAccountRiskNotificationClaimDisableFailureAndRestart(t *testing.T) {
	for _, scenario := range []string{"off", "detection off", "database failure", "claim busy", "send failure", "outcome save failure", "claimed crash"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.NotifyEnabled = true
			riskRecordStop(r, &accounts.pool[0], now, "confirmed stop")
			n := &riskNoticeStub{}
			s.notifier = n
			switch scenario {
			case "off":
				r.Config.NotifyEnabled = false
			case "detection off":
				r.Config.Enabled = false
			case "database failure":
				store.fail = true
			case "claim busy":
				store.busy = 1
			case "send failure":
				n.err = errors.New("unconfirmed https://secret")
			case "outcome save failure":
				n.after = func() { store.fail = true }
			case "claimed crash":
				r.Events[0].Notification = "sending"
				r.Events[0].NotificationAt = &now
			}
			_ = s.deliverNotification(context.Background(), now)
			if scenario == "send failure" || scenario == "outcome save failure" {
				require.Len(t, n.messages, 1)
			} else {
				require.Empty(t, n.messages)
			}
			if scenario == "send failure" {
				require.Equal(t, "failed", r.Events[0].Notification)
			}
			store.fail = false
			if scenario != "database failure" && scenario != "claim busy" {
				require.NoError(t, s.deliverNotification(context.Background(), now.Add(time.Minute)))
				if scenario == "send failure" || scenario == "outcome save failure" {
					require.Len(t, n.messages, 1)
				} else {
					require.Empty(t, n.messages)
				}
			}
			if scenario == "claimed crash" || scenario == "outcome save failure" {
				require.Equal(t, "unknown", r.Events[0].Notification)
			}
			raw, _ := json.Marshal(r)
			require.NotContains(t, string(raw), "https://secret")
		})
	}
}

func TestAccountRiskNotificationConcurrentInstancesAndBoundedQueue(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.NotifyEnabled = true
	for i := 0; i < 70; i++ {
		riskRecordStop(r, &accounts.pool[0], now.Add(time.Duration(i)*time.Millisecond), "confirmed")
	}
	require.Len(t, r.Events, 50)
	n := &riskNoticeStub{}
	s.notifier = n
	s2 := &AccountRiskService{store: store, groups: s.groups, notifier: n}
	var wg sync.WaitGroup
	for _, worker := range []*AccountRiskService{s, s2} {
		wg.Add(1)
		go func(w *AccountRiskService) { defer wg.Done(); _ = w.deliverNotification(context.Background(), now) }(worker)
	}
	wg.Wait()
	require.Len(t, n.messages, 1)
	require.NoError(t, s2.deliverNotification(context.Background(), now.Add(time.Second)))
	require.Len(t, n.messages, 1)
	require.NoError(t, s2.deliverNotification(context.Background(), now.Add(3*time.Second)))
	require.Len(t, n.messages, 2)
}

func TestAccountRiskNotificationReusesRefillWebhookWithPurchasesOff(t *testing.T) {
	refill, store, provider, _ := refillHarness(t)
	refill.encryptor = &plainEncryptor{}
	ctx := context.Background()
	c := refillTestConfig()
	c.Enabled, c.MonitorEnabled = false, false
	c.Notifications = AccountRefillNotificationConfig{}
	require.NoError(t, refill.Configure(ctx, c, "", 1, fixtureRefillWebhook))
	before := store.raw
	var bodies []string
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "text", payload.MsgType)
		bodies = append(bodies, payload.Text.Content)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer receiver.Close()
	client := receiver.Client()
	tr := client.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1" // Validate the isolated receiver's certificate.
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", receiver.Listener.Addr().String())
	}
	client.Transport = tr
	defer tr.CloseIdleConnections()
	refill.notifier = weComRefillNotifier{client: client}
	ready, err := refill.RiskWebhookConfigured(ctx, c.GroupID)
	require.NoError(t, err)
	require.True(t, ready)
	require.NoError(t, refill.SendRiskNotification(ctx, c.GroupID, "confirmed risk stop"))
	require.Equal(t, []string{"confirmed risk stop"}, bodies)
	require.Equal(t, before, store.raw)
	require.Empty(t, provider.submitted)
	store.mutate(t, func(v *accountRefillState) { v.EncryptedWebhook = "" })
	ready, err = refill.RiskWebhookConfigured(ctx, c.GroupID)
	require.NoError(t, err)
	require.False(t, ready)
	require.Error(t, refill.SendRiskNotification(ctx, c.GroupID, "not sent"))
	require.Len(t, bodies, 1)
}

func TestAccountRiskNotificationViewAndLegacyDefault(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	require.False(t, DefaultAccountRiskConfig().NotifyEnabled)
	riskRecordStop(store.state.Rules[7], &accounts.pool[0], now, "historic")
	store.state.Rules[7].Config.NotifyEnabled = true
	n := &riskNoticeStub{}
	s.notifier = n
	require.NoError(t, s.deliverNotification(context.Background(), now))
	require.Empty(t, n.messages)
	v, err := s.View(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, "ready", v.WebhookStatus)
	raw, _ := json.Marshal(v)
	require.Contains(t, string(raw), `"notify_enabled":true`)
	require.NotContains(t, string(raw), "encrypted_webhook")
	require.Equal(t, 100, len([]rune(riskNotificationName(strings.Repeat("长", 120)))))
}
