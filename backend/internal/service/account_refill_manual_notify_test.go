//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMuzeTimeoutAndSuccessUseSameOrderAndDoNotRepeat(t *testing.T) {
	for _, mode := range []string{"normal", "lost-receipt", "resolved-same-tick"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Now()))
				s, store, provider, accounts := muzeHarness(t)
				p := &muzeRecoveryProvider{muzeTestProvider: provider}
				s.muze = p
				if mode != "normal" {
					provider.orderError = errors.New("lost response")
				}
				n := &refillNoticeStub{}
				s.notifier = n
				store.mutate(t, func(v *accountRefillState) {
					v.Config.Notifications.RefillStarted = true
					v.EncryptedWebhook = "encrypted-fixture"
				})
				ctx := context.Background()
				require.NoError(t, s.Manual(ctx, muzeRequest()))
				require.NoError(t, s.run(ctx, false))
				v, err := s.View(ctx)
				require.NoError(t, err)
				id := v.Jobs[0].ID
				require.Len(t, n.messages, 1)
				require.Contains(t, n.messages[0], "muze手动补号")
				if mode == "resolved-same-tick" {
					time.Sleep(11 * time.Minute)
					store.mutate(t, func(v *accountRefillState) {
						v.Config.Enabled, v.Config.MonitorEnabled = true, true
						v.ProviderRetryAt = time.Now().Add(time.Hour)
					})
				} else {
					time.Sleep(5 * time.Minute)
					refillDue(t, store)
					require.NoError(t, s.run(ctx, false))
					require.Len(t, n.messages, 2)
					require.Contains(t, n.messages[1], "muze手动补号超时提醒")
					require.Contains(t, n.messages[1], "号商任务：")
					require.NotContains(t, n.messages[1], id)
				}
				pushMuzeFixture(provider, accounts)
				p.found = provider.cycle
				store.mutate(t, func(v *accountRefillState) { v.Jobs[0].NextAttemptAt = time.Time{} })
				require.NoError(t, s.run(ctx, false))
				wantMessages := 3
				if mode == "resolved-same-tick" {
					wantMessages = 2
				}
				require.Len(t, n.messages, wantMessages)
				success := n.messages[len(n.messages)-1]
				require.Contains(t, success, "muze手动补号成功")
				require.NotContains(t, success, id)
				require.Contains(t, success, "号商任务：rvc_fixture")
				require.Contains(t, success, "预购次数：1")
				require.NotContains(t, success, "积分：")
				v, err = s.View(ctx)
				require.NoError(t, err)
				require.Equal(t, id, v.Jobs[0].ID, "the delayed order itself must complete")
				require.NotNil(t, v.Jobs[0].ImportedNotificationAt)
				if mode == "resolved-same-tick" {
					require.Nil(t, v.Jobs[0].DelayedNotificationAt)
					require.Nil(t, v.Jobs[0].TimeoutNotificationAt)
				}
				restarted := *s
				time.Sleep(15 * time.Minute)
				require.NoError(t, restarted.run(ctx, false))
				require.Len(t, n.messages, wantMessages)
				require.Equal(t, 1, provider.orders)
				require.Equal(t, 1, accounts.adopt)
			})
		})
	}
}

func TestManualRefillDelaySharesProgressSwitchAndSkipsDeliveredAccounts(t *testing.T) {
	for _, provider := range []string{"", refillProviderMuze} {
		for _, mode := range []string{"waiting", "already-pushed", "switch-off"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				n := &refillNoticeStub{}
				s := &AccountRefillService{notifier: n, encryptor: refillTestEncryptor{}}
				now := time.Now()
				state := accountRefillState{Config: AccountRefillConfig{Notifications: AccountRefillNotificationConfig{RefillStarted: mode != "switch-off"}}, EncryptedWebhook: "encrypted-fixture",
					Jobs: []AccountRefillJob{{ID: "local-job", Provider: provider, ManualRequestID: "request", Status: "processing", CreatedAt: now.Add(-6 * time.Minute)}}}
				if mode == "already-pushed" {
					state.Jobs[0].Status, state.Jobs[0].AccountID = "awaiting_push", 344
				}
				require.NoError(t, s.notifyRefillDelayed(context.Background(), &state, now, func() error { return nil }))
				if mode == "waiting" {
					require.Len(t, n.messages, 1)
					require.Contains(t, n.messages[0], "手动补号超时提醒")
					require.Contains(t, n.messages[0], "号商暂未返回 ID")
					require.NotContains(t, n.messages[0], "local-job")
				} else {
					require.Empty(t, n.messages)
				}
			})
		}
	}
}
