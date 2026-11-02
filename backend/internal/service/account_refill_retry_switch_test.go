//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillImmediateRetrySwitchPersistsAndPreservesLegacySaves(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	ctx := context.Background()
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, refillImmediateFailureRetryEnabled(v.Config))
	disabled := false
	c := v.Config
	c.ImmediateFailureRetry = &disabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	// Simulate an older client saving another field without the new switch.
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.NotNil(t, v.Config.ImmediateFailureRetry)
	require.False(t, *v.Config.ImmediateFailureRetry)
	c = v.Config
	c.ImmediateFailureRetry = nil
	c.CooldownSeconds++
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	// A new service instance must read the same explicit false from storage.
	s = &AccountRefillService{store: store, provider: s.provider, encryptor: s.encryptor, accounts: s.accounts, admin: s.admin, groups: s.groups, loads: s.loads}
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.False(t, refillImmediateFailureRetryEnabled(v.Config))
	encoded, err := json.Marshal(v.Config)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"immediate_failure_retry":false`)
	enabled := true
	c = v.Config
	c.ImmediateFailureRetry = &enabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.True(t, refillImmediateFailureRetryEnabled(v.Config))
}

func TestRefillImmediateRetrySwitchIsScopedToGroup(t *testing.T) {
	s, base, _, _ := failureRefillHarness(t)
	s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	ctx := context.Background()
	v, err := s.View(ctx)
	require.NoError(t, err)
	disabled := false
	c := v.Config
	c.ImmediateFailureRetry = &disabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	childView, err := child.View(ctx)
	require.NoError(t, err)
	require.True(t, refillImmediateFailureRetryEnabled(childView.Config))
	require.NoError(t, child.Configure(ctx, childView.Config, "", childView.Revision))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.False(t, refillImmediateFailureRetryEnabled(v.Config), "saving another group must not re-enable this group")
}

type refillRetryPollingProvider struct {
	*refillTimingProvider
	statusCalls int
}

func (p *refillRetryPollingProvider) Status(_ context.Context, _ string, id string) (*RefillProviderJob, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statusCalls++
	return &RefillProviderJob{ID: id, Status: "failed"}, nil
}

func (p *refillRetryPollingProvider) queries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusCalls
}

func TestRefillImmediateRetrySwitchWorkerTiming(t *testing.T) {
	for _, tc := range []struct {
		name, mode      string
		enabled         bool
		check, cooldown int
		secondAt        time.Duration
	}{
		{"enabled bypasses both", "fixed", true, 60, 300, 20 * time.Second},
		{"disabled five seconds", "fixed", false, 5, 5, 20 * time.Second},
		{"disabled waits check", "fixed", false, 60, 5, 65 * time.Second},
		{"disabled waits cooldown", "fixed", false, 5, 30, 35 * time.Second},
		{"disabled waits both", "fixed", false, 60, 300, 305 * time.Second},
		{"disabled interval restores original quota", "interval", false, 5, 30, 35 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base, original, _ := failureRefillHarness(t)
				base.mutate(t, func(state *accountRefillState) {
					state.Config.Mode = tc.mode
					state.Config.ImmediateFailureRetry = &tc.enabled
					state.Config.CheckSeconds, state.Config.CooldownSeconds = tc.check, tc.cooldown
					state.NextIntervalAt = time.Now().Add(-time.Second)
				})
				store := &refillTimingStore{raw: base.raw}
				provider := &refillRetryPollingProvider{refillTimingProvider: &refillTimingProvider{failureRefillProvider: original}}
				s.store, s.provider = store, provider
				s.stop = make(chan struct{})
				s.Start()
				defer s.Stop()
				synctest.Wait()
				time.Sleep(5 * time.Second)
				synctest.Wait()
				orders := provider.submittedOrders()
				require.Len(t, orders, 1)
				first := orders[0]
				time.Sleep(15*time.Second - time.Nanosecond)
				synctest.Wait()
				require.Zero(t, provider.queries(), "five-second demand checks must not poll supplier orders early")
				require.Len(t, provider.submittedOrders(), 1)
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				require.Equal(t, 1, provider.queries(), "query the accepted order 15 seconds after submission")
				if tc.secondAt > 20*time.Second {
					require.Len(t, provider.submittedOrders(), 1)
					time.Sleep(tc.secondAt - 20*time.Second - time.Nanosecond)
					synctest.Wait()
					require.Len(t, provider.submittedOrders(), 1, "disabled shortcut must honor both normal deadlines")
					time.Sleep(time.Nanosecond)
					synctest.Wait()
				}
				orders = provider.submittedOrders()
				require.Len(t, orders, 2)
				require.Equal(t, first.ID, orders[1].ReplacesJobID)
				require.Equal(t, first.Plan, orders[1].Plan)
				require.NotEqual(t, first.ID, orders[1].ID)
				raw, err := store.Read(context.Background())
				require.NoError(t, err)
				state, err := decodeAccountRefillState(raw)
				require.NoError(t, err)
				require.Equal(t, 1, state.RotationCursor, "replacement must not advance rotation even when delayed")
				require.Zero(t, state.IntervalRemaining)
				require.Equal(t, orders[1].ID, state.Jobs[1].ReplacementJobID)
			})
		})
	}
}

func TestRefillImmediateRetrySwitchCanDisableAnAlreadyPendingReplacement(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled = false
		state.Jobs[0].NextAttemptAt = time.Time{}
	})
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, v.Jobs[0].ReplacementPending)
	disabled := false
	c := v.Config
	c.Enabled, c.ImmediateFailureRetry = true, &disabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "turning the shortcut off also gates an existing pending replacement")
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Contains(t, v.Message, "冷却")
	enabled := true
	c = v.Config
	c.ImmediateFailureRetry = &enabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
}

func TestRefillImmediateRetrySwitchFailureNotificationMatchesSavedChoice(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "immediate"}[enabled], func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			notices := &refillNoticeStub{}
			s.notifier = notices
			store.mutate(t, func(state *accountRefillState) {
				state.Config.ImmediateFailureRetry = &enabled
				state.Config.Notifications.RefillStarted = true
				state.EncryptedWebhook = "encrypted-fixture"
			})
			require.NoError(t, s.run(context.Background(), false))
			p.states[p.orders[0].ID] = "failed"
			store.mutate(t, func(state *accountRefillState) { state.Jobs[0].NextAttemptAt = time.Time{} })
			require.NoError(t, s.run(context.Background(), false))
			require.GreaterOrEqual(t, len(notices.messages), 2)
			if enabled {
				require.Contains(t, notices.messages[1], "跳过普通下单冷却")
			} else {
				require.Contains(t, notices.messages[1], "立即重补已关闭")
				require.Contains(t, notices.messages[1], "遵守普通检测间隔和下单冷却")
				require.NotContains(t, notices.messages[1], "跳过")
			}
		})
	}
}
