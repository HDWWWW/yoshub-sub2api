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

func TestRefillNeededNotificationWithAutomationOffIsDurableAndNeverOrders(t *testing.T) {
	s, store, provider, accounts := refillHarness(t)
	ctx := context.Background()
	n := &refillNoticeStub{}
	s.notifier = n
	c := refillTestConfig()
	c.Enabled = false
	c.Notifications.RefillNeeded = true
	require.Error(t, s.Configure(ctx, c, "", 1), "this notification also requires a robot")
	require.NoError(t, s.Configure(ctx, c, "", 1, fixtureRefillWebhook))
	store.mutate(t, func(state *accountRefillState) {
		state.EncryptedToken = ""
		state.ProviderRetryAt = time.Now().Add(time.Hour)
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "自动补号已关闭，未自动补号")
	require.Contains(t, n.messages[0], "监测分组：#2")
	require.Regexp(t, `北京时间：\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \(UTC\+08:00\)`, n.messages[0])
	require.Contains(t, n.messages[0], "健康储备不足")
	require.Contains(t, n.messages[0], "当前待补：1 个")
	require.NotContains(t, n.messages[0], "开始补号")
	// A new service instance still sees the successful persisted notification.
	s = &AccountRefillService{store: store, provider: provider, notifier: n, encryptor: s.encryptor, accounts: accounts, groups: s.groups, loads: s.loads}
	for i := 0; i < 2; i++ {
		store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, n.messages, 1)
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(4 * time.Hour))}
	store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
	require.NoError(t, s.run(ctx, false))
	accounts.accounts = nil
	store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 2, "recovered demand rearms the alert")
	require.Empty(t, provider.submitted)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.Jobs)
	require.Empty(t, state.Usage)
	require.Zero(t, state.RotationCursor)
}

func TestRefillNeededNotificationRequiresMonitorTriggerAndNonPreview(t *testing.T) {
	for _, scenario := range []string{"switch_off", "automation_on", "monitor_off", "out_of_window", "pending", "attention", "preview", "get", "persist_failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := refillHarness(t)
			n := &refillNoticeStub{}
			s.notifier = n
			store.mutate(t, func(state *accountRefillState) {
				state.Config.Enabled = false
				state.Config.Notifications.RefillNeeded = true
				state.EncryptedWebhook = "encrypted-fixture"
				state.EncryptedToken = ""
				state.ProviderRetryAt = time.Now().Add(time.Hour)
				switch scenario {
				case "switch_off":
					state.Config.Notifications.RefillNeeded = false
				case "automation_on":
					state.Config.Enabled = true
				case "monitor_off":
					state.Config.MonitorEnabled = false
				case "out_of_window":
					state.Config.WindowEnabled = true
					state.Config.WindowStart = time.Now().In(refillTimezone).Add(2 * time.Hour).Format("15:04")
					state.Config.WindowEnd = time.Now().In(refillTimezone).Add(3 * time.Hour).Format("15:04")
				case "pending":
					state.Jobs = []AccountRefillJob{{Status: "queued", Concurrency: 60}}
				case "attention":
					state.Jobs = []AccountRefillJob{{Status: "review"}}
				}
			})
			store.failSave = scenario == "persist_failure"
			var err error
			if scenario == "get" {
				_, err = s.View(context.Background())
			} else {
				err = s.run(context.Background(), scenario == "preview")
			}
			if scenario == "persist_failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, n.messages)
			require.Empty(t, p.submitted)
		})
	}
}

func TestRefillNeededNotificationWaitsForCapacityAndForecastObservation(t *testing.T) {
	for _, forecast := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "forecast"}[forecast], func(t *testing.T) {
			s, store, p, accounts := refillHarness(t)
			n := &refillNoticeStub{}
			s.notifier = n
			accounts.accounts = []Account{refillTestAccount(time.Now().Add(4 * time.Hour))}
			s.loads = refillTestLoads{map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}}
			if forecast {
				accounts.accounts = refillForecastAccounts(time.Now())
				s.loads = refillTestLoads{refillForecastLoads()}
			}
			store.mutate(t, func(state *accountRefillState) {
				state.Config.Enabled = false
				state.Config.Notifications.RefillNeeded = true
				state.EncryptedWebhook = "encrypted-fixture"
				state.ProviderRetryAt = time.Now().Add(time.Hour)
			})
			require.NoError(t, s.run(context.Background(), false))
			require.Empty(t, n.messages)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			feedRefillForecast(s, state.Config, state.Revision, state.Evaluation, time.Now(), state.Config.ConcurrencySustainSeconds)
			store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, n.messages, 1)
			require.Contains(t, n.messages[0], "持续观察已满 30 秒")
			require.Empty(t, p.submitted)
		})
	}
}

func TestRefillNeededNotificationRetriesWithPersistentCooldown(t *testing.T) {
	s, store, p, accounts := refillHarness(t)
	n := &refillNoticeStub{err: errors.New("robot offline")}
	s.notifier = n
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled = false
		state.Config.Notifications.RefillNeeded = true
		state.EncryptedWebhook = "encrypted-fixture"
		state.ProviderRetryAt = time.Now().Add(time.Hour)
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 1)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.RefillNeededRetryAt.After(time.Now().Add(4*time.Minute)))
	require.Contains(t, state.NotificationStatus.Message, "自动补号已关闭，未下单")
	require.NotContains(t, state.NotificationStatus.Message, "补号继续处理")
	s = &AccountRefillService{store: store, provider: p, notifier: n, encryptor: s.encryptor, accounts: accounts, groups: s.groups, loads: s.loads}
	store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 1, "do not retry on every monitor tick")
	n.err = nil
	store.mutate(t, func(state *accountRefillState) {
		state.NextNeededCheckAt = time.Time{}
		state.RefillNeededRetryAt = time.Now().Add(-time.Second)
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 2)
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.RefillNeededRetryAt.IsZero())
	require.Empty(t, p.submitted)
}

func TestRefillNeededIntervalRetryDoesNotImmediatelyRepeatSuccessfulAlert(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	n := &refillNoticeStub{err: errors.New("robot offline")}
	s.notifier = n
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled = false
		state.Config.Mode = "interval"
		state.Config.Notifications.RefillNeeded = true
		state.EncryptedWebhook = "encrypted-fixture"
		state.ProviderRetryAt = time.Now().Add(time.Hour)
		state.NextIntervalAt = time.Now().Add(-time.Minute)
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 1)
	n.err = nil
	store.mutate(t, func(state *accountRefillState) {
		state.NextNeededCheckAt = time.Time{}
		state.NextNeededIntervalAt = time.Now().Add(-time.Minute)
		state.RefillNeededRetryAt = time.Now().Add(-time.Minute)
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 2)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.NextNeededIntervalAt.After(time.Now().Add(time.Duration(state.Config.IntervalMinutes-1)*time.Minute)))
	store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 2, "a successful retry must not be followed by another alert on the next tick")
	require.Empty(t, p.submitted)
}

func TestRefillNeededIntervalNotificationDoesNotAccumulatePurchases(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	n := &refillNoticeStub{}
	s.notifier = n
	purchaseDeadline := time.Now().Add(-time.Minute)
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled = false
		state.Config.Mode = "interval"
		state.Config.Notifications.RefillNeeded = true
		state.EncryptedWebhook = "encrypted-fixture"
		state.ProviderRetryAt = time.Now().Add(time.Hour)
		state.NextIntervalAt = purchaseDeadline
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "定时补号")
	store.mutate(t, func(state *accountRefillState) { state.NextNeededCheckAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 1)
	store.mutate(t, func(state *accountRefillState) {
		state.NextNeededCheckAt = time.Time{}
		state.NextNeededIntervalAt = time.Now().Add(-time.Minute)
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, n.messages, 2)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.NextIntervalAt.Equal(purchaseDeadline))
	require.Zero(t, state.IntervalRemaining)
	require.Empty(t, state.Jobs)
	require.Empty(t, state.Usage)
	require.Empty(t, p.submitted)
}

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

func TestRefillFailedNotificationIsDurableAndDoesNotBlockReplacement(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	n := &refillNoticeStub{err: errors.New("robot unavailable")}
	s.notifier = n
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Notifications.RefillStarted = true
		v.EncryptedWebhook = "encrypted-fixture"
	})
	require.NoError(t, s.run(ctx, false))
	first := p.orders[0]
	p.states[first.ID] = "failed"
	refillDue(t, store)
	p.beforeSubmit = func(j AccountRefillJob) {
		v, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.NotNil(t, v.Jobs[1].FailedNotificationAt)
	}
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2, "notification errors must not block same-plan replacement")
	require.Len(t, n.messages, 3, "start, confirmed failure, then replacement start")
	require.Contains(t, n.messages[1], "Sub2API 补号失败")
	require.Contains(t, n.messages[1], "套餐：pro20x")
	require.Contains(t, n.messages[1], first.ID)
	require.Contains(t, n.messages[1], "不推进轮转、不叠加补号")
	require.NotContains(t, n.messages[1], "encrypted-fixture")
	s = &AccountRefillService{store: store, provider: p, notifier: n, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	for i := 0; i < 3; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, n.messages, 3, "no repeat after restart or polling")
	p.states[p.orders[1].ID] = "failed"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 5, "the replacement failure gets its own notification")
}

func TestRefillFailedNotificationRequiresConfirmedResultAndEnabledSwitch(t *testing.T) {
	for _, scenario := range []string{"manual", "disabled", "no_webhook", "review", "timeout", "persist_failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			ctx := context.Background()
			n := &refillNoticeStub{}
			s.notifier = n
			require.NoError(t, s.run(ctx, false))
			p.states[p.orders[0].ID] = "failed"
			refillDue(t, store)
			store.mutate(t, func(v *accountRefillState) {
				v.Config.Enabled = false
				v.Config.Notifications.RefillStarted = scenario != "disabled"
				v.EncryptedWebhook = "encrypted-fixture"
				switch scenario {
				case "manual":
					v.Jobs[0].ManualRequestID, v.Jobs[0].Reason = "manual-request", "手动补号"
				case "no_webhook":
					v.EncryptedWebhook = ""
				case "review":
					p.states[p.orders[0].ID] = "review"
				case "timeout":
					p.statusErr = errors.New("uncertain network timeout")
				}
			})
			store.failSave = scenario == "persist_failure"
			err := s.run(ctx, false)
			if scenario == "persist_failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if scenario == "manual" {
				require.Len(t, n.messages, 1)
				require.Contains(t, n.messages[0], "手动订单不自动重下")
			} else {
				require.Empty(t, n.messages)
			}
			require.Len(t, p.orders, 1)
		})
	}
}
