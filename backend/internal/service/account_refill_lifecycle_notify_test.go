//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillImportedNotificationOnlyAfterImportAndOnce(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	n := &refillNoticeStub{err: errors.New("robot unavailable")}
	s.notifier = n
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Notifications.RefillStarted = true
		v.EncryptedWebhook = "encrypted-fixture"
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 1)
	first := p.orders[0]
	p.states[first.ID] = "succeeded"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 1, "supplier success is not an imported account")
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, accounts.creates)
	require.Len(t, n.messages, 2)
	require.Contains(t, n.messages[1], "自动补号成功")
	require.Contains(t, n.messages[1], "账号已导入")
	require.Contains(t, n.messages[1], first.ID)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "imported", v.Jobs[0].Status)
	require.NotNil(t, v.Jobs[0].ImportedNotificationAt)
	s = &AccountRefillService{store: store, provider: p, notifier: n, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 2)
	require.Equal(t, 1, accounts.creates)
}

func TestRefillDelayedNotificationRunsDuringBackoffAndPausedAutomation(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	n := &refillNoticeStub{}
	s.notifier = n
	require.NoError(t, s.run(ctx, false))
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Notifications.RefillStarted = true
		v.EncryptedWebhook = "encrypted-fixture"
		v.Config.Enabled, v.Config.MonitorEnabled, v.Config.PushEnabled = false, false, false
		v.ProviderRetryAt = time.Now().Add(time.Hour)
		v.NextCheckAt = time.Now().Add(time.Hour)
		v.Jobs[0].CreatedAt = time.Now().Add(-5 * time.Minute)
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "5 分钟")
	require.Contains(t, n.messages[0], p.orders[0].ID)
	require.Len(t, p.orders, 1, "a delay notice must not place another order")
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NotNil(t, v.Jobs[0].DelayedNotificationAt)
	s = &AccountRefillService{store: store, provider: p, notifier: n, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	for i := 0; i < 3; i++ {
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, n.messages, 1, "restart and repeated checks must not resend the alert")
	// Eventually importing the delayed order still sends its success notice.
	store.mutate(t, func(v *accountRefillState) { v.Config.PushEnabled = true })
	p.states[p.orders[0].ID] = "succeeded"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, n.messages, 2)
	require.Contains(t, n.messages[1], "自动补号成功")
}

func TestRefillDelayedNotificationThresholdAndStates(t *testing.T) {
	for _, tc := range []struct {
		name, status                                        string
		age                                                 time.Duration
		manual, disabled, noWebhook, saveFailed, sendFailed bool
		want                                                bool
	}{
		{name: "before threshold", status: "processing", age: 5*time.Minute - time.Second},
		{name: "at threshold", status: "processing", age: 5 * time.Minute, want: true},
		{name: "queued", status: "queued", age: 6 * time.Minute, want: true},
		{name: "review", status: "review", age: 6 * time.Minute, want: true},
		{name: "blocked", status: "blocked", age: 6 * time.Minute, want: true},
		{name: "import failed", status: "import_failed", age: 6 * time.Minute, want: true},
		{name: "awaiting delivery", status: "awaiting_delivery", age: 6 * time.Minute, want: true},
		{name: "imported", status: "imported", age: 6 * time.Minute},
		{name: "failed", status: "failed", age: 6 * time.Minute},
		{name: "expired", status: "delivery_expired", age: 6 * time.Minute},
		{name: "dismissed", status: "dismissed", age: 6 * time.Minute},
		{name: "manual", status: "processing", age: 6 * time.Minute, manual: true},
		{name: "off", status: "processing", age: 6 * time.Minute, disabled: true},
		{name: "no webhook", status: "processing", age: 6 * time.Minute, noWebhook: true},
		{name: "save failed", status: "processing", age: 6 * time.Minute, saveFailed: true},
		{name: "send failed", status: "processing", age: 6 * time.Minute, sendFailed: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			n := &refillNoticeStub{}
			if tc.sendFailed {
				n.err = errors.New("robot timeout")
			}
			s := &AccountRefillService{notifier: n, encryptor: refillTestEncryptor{}}
			state := accountRefillState{Config: AccountRefillConfig{Notifications: AccountRefillNotificationConfig{RefillStarted: !tc.disabled}},
				EncryptedWebhook: "encrypted-fixture", Jobs: []AccountRefillJob{{ID: "order-1", Status: tc.status, CreatedAt: now.Add(-tc.age)}}}
			if tc.noWebhook {
				state.EncryptedWebhook = ""
			}
			if tc.manual {
				state.Jobs[0].ManualRequestID = "manual-request"
			}
			persisted := false
			persist := func() error {
				if tc.saveFailed {
					return errors.New("save failed")
				}
				persisted = true
				return nil
			}
			err := s.notifyRefillDelayed(context.Background(), &state, now, persist)
			if tc.saveFailed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.want {
				require.True(t, persisted)
				require.Len(t, n.messages, 1)
				require.NoError(t, s.notifyRefillDelayed(context.Background(), &state, now.Add(time.Hour), persist))
				require.Len(t, n.messages, 1)
			} else {
				require.Empty(t, n.messages)
			}
		})
	}
}

func TestRefillImportedNotificationRequiresDurableAutomaticImport(t *testing.T) {
	for _, scenario := range []string{"manual", "disabled", "no_webhook", "not_imported", "no_account", "persist_failure"} {
		t.Run(scenario, func(t *testing.T) {
			n := &refillNoticeStub{}
			s := &AccountRefillService{notifier: n, encryptor: refillTestEncryptor{}}
			state := accountRefillState{Config: AccountRefillConfig{Notifications: AccountRefillNotificationConfig{RefillStarted: true}}, EncryptedWebhook: "encrypted-fixture"}
			j := AccountRefillJob{Status: "imported", AccountID: 7, CreatedAt: time.Now()}
			switch scenario {
			case "manual":
				j.Reason = "手动补号"
			case "disabled":
				state.Config.Notifications.RefillStarted = false
			case "no_webhook":
				state.EncryptedWebhook = ""
			case "not_imported":
				j.Status = "awaiting_delivery"
			case "no_account":
				j.AccountID = 0
			}
			err := s.notifyRefillImported(context.Background(), &state, &j, func() error { return errors.New("save failed") })
			if scenario == "persist_failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, n.messages)
		})
	}
}
