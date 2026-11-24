//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fallbackPrimaryJob(id, status string, created time.Time) AccountRefillJob {
	j := AccountRefillJob{ID: id, SupplierID: "supplier-" + id, Status: status, SupplierStatus: status,
		GroupID: 2, Mode: "fixed", Plan: "pro5x", DeliveryGroupIDs: []int64{2}, Concurrency: 60,
		CreatedAt: created, UpdatedAt: created, NextAttemptAt: time.Now().Add(time.Hour)}
	if status == "failed" || status == "imported" {
		j.FinishedAt = &created
		j.BudgetReleased = status == "failed"
		j.ReplacementPending = status == "failed"
		if status == "imported" {
			j.AccountID = 42
		}
	}
	return j
}

func fallbackHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *muzeTestProvider, *muzeTestAdmin) {
	s, store, p, accounts := muzeHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled, state.Config.MonitorEnabled = true, true
		state.Config.Mode = "fixed"
		state.Config.MuzeFallback = &AccountRefillMuzeFallbackConfig{true, 3, 5}
		state.ProviderRetryAt, state.NextCheckAt = time.Now().Add(time.Hour), time.Now().Add(time.Hour)
		state.Config.PlanImportConcurrency = map[string]int{"pro20x": 77}
		state.Config.ImportExpiryGraceMinutes = 10
		state.Config.Delivery.Priority = 8
	})
	return s, store, p, accounts
}

func TestMuzeFallbackFailureThresholdAndSuccessReset(t *testing.T) {
	for _, tc := range []struct {
		name             string
		count, threshold int
		successAfter     bool
		want             int
	}{{"two-failures", 2, 3, false, 0}, {"third-failure", 3, 3, false, 1}, {"custom-threshold", 2, 2, false, 1}, {"success-resets", 3, 3, true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, p, _ := fallbackHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				state.Config.MuzeFallback.FailureThreshold = tc.threshold
				for i := 0; i < tc.count; i++ {
					state.Jobs = append(state.Jobs, fallbackPrimaryJob(fmt.Sprint(i), "failed", time.Now().Add(time.Duration(i-10)*time.Minute)))
				}
				if tc.successAfter {
					state.Jobs = append(state.Jobs, fallbackPrimaryJob("success", "imported", time.Now().Add(-time.Minute)))
				}
			})
			require.NoError(t, s.run(context.Background(), false))
			require.Equal(t, tc.want, p.orders)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			if tc.want == 1 {
				j := state.Jobs[0]
				require.Equal(t, refillProviderMuze, j.Provider)
				require.Equal(t, "pro20x", j.Plan)
				require.Equal(t, 77, j.Concurrency)
				require.Equal(t, 8, j.DeliveryConfig.Priority)
				require.Equal(t, 10, *j.ExpiryGraceMinutes)
				require.Empty(t, j.ManualRequestID)
				require.NotEmpty(t, j.FallbackForJobID)
				require.Equal(t, 1, state.Usage[time.Now().In(refillTimezone).Format("2006-01-02")].Count)
				require.Zero(t, state.Usage[time.Now().In(refillTimezone).Format("2006-01-02")].Points)
				require.Empty(t, refillMuzeFailures(&state))
				require.Equal(t, -1, nextRefillFailureReplacement(&state))
			}
		})
	}
}

func TestMuzeFallbackAnySuccessfulRefillRestartsFailureCount(t *testing.T) {
	for _, scenario := range []string{"manual-primary", "manual-muze", "forced-muze", "daily-primary", "daily-muze"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := fallbackHarness(t)
			now := time.Now()
			store.mutate(t, func(state *accountRefillState) {
				state.Config.MuzeFallback.FailureThreshold = 2
				state.Jobs = []AccountRefillJob{
					fallbackPrimaryJob("old-first", "failed", now.Add(-10*time.Minute)),
					fallbackPrimaryJob("old-second", "failed", now.Add(-9*time.Minute)),
				}
				success := fallbackPrimaryJob("success", "imported", now.Add(-8*time.Minute))
				// Manual and scheduled orders do not carry the automatic rule's mode.
				success.Mode = ""
				switch scenario {
				case "manual-primary":
					success.ManualRequestID = "manual"
				case "manual-muze", "forced-muze":
					success.Provider, success.ManualRequestID = refillProviderMuze, "manual"
					success.Force = scenario == "forced-muze"
				case "daily-primary", "daily-muze":
					success.DailyBatchID = "daily"
					if scenario == "daily-muze" {
						success.Provider = refillProviderMuze
					}
				}
				state.Jobs = append(state.Jobs, success)
			})
			// Load persisted history through a fresh service instance, as after a restart.
			restarted := *s
			view, err := restarted.View(context.Background())
			require.NoError(t, err)
			require.Zero(t, view.MuzeFallbackStatus.ConsecutiveFailures)
			require.NoError(t, restarted.run(context.Background(), false))
			require.Zero(t, p.orders, "old failures must not buy a backup after a successful manual or scheduled import")

			store.mutate(t, func(state *accountRefillState) {
				state.Jobs = append(state.Jobs, fallbackPrimaryJob("new-first", "failed", now.Add(-4*time.Minute)))
			})
			view, err = restarted.View(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, view.MuzeFallbackStatus.ConsecutiveFailures)
			require.NoError(t, restarted.run(context.Background(), false))
			require.Zero(t, p.orders, "one failure after success is below the configured threshold")

			store.mutate(t, func(state *accountRefillState) {
				state.Jobs = append(state.Jobs, fallbackPrimaryJob("new-second", "failed", now.Add(-time.Minute)))
			})
			require.NoError(t, restarted.run(context.Background(), false))
			require.Equal(t, 1, p.orders, "two new failures permit one backup")
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, "new-second", state.Jobs[0].FallbackForJobID)
			require.Empty(t, refillMuzeFailures(&state))
		})
	}
}

func TestMuzeFallbackSuccessResetRequiresImportedAccountInSameGroup(t *testing.T) {
	for _, scenario := range []string{"other-group", "awaiting-delivery", "no-account", "unfinished", "success-before-failures"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, _, _ := fallbackHarness(t)
			now := time.Now()
			store.mutate(t, func(state *accountRefillState) {
				state.Config.MuzeFallback.FailureThreshold = 2
				state.Jobs = []AccountRefillJob{
					fallbackPrimaryJob("first", "failed", now.Add(-3*time.Minute)),
					fallbackPrimaryJob("second", "failed", now.Add(-2*time.Minute)),
				}
				success := fallbackPrimaryJob("success", "imported", now.Add(-time.Minute))
				success.Provider, success.ManualRequestID = refillProviderMuze, "manual"
				switch scenario {
				case "other-group":
					success.GroupID = 3
				case "awaiting-delivery":
					success.Status, success.AccountID = "awaiting_delivery", 0
				case "no-account":
					success.AccountID = 0
				case "unfinished":
					success.FinishedAt = nil
				case "success-before-failures":
					finished := now.Add(-4 * time.Minute)
					success.FinishedAt = &finished
				}
				state.Jobs = append(state.Jobs, success)
			})
			view, err := s.View(context.Background())
			require.NoError(t, err)
			require.Equal(t, 2, view.MuzeFallbackStatus.ConsecutiveFailures)
		})
	}
}

func TestMuzeFallbackTimeoutBoundaryAndOnePurchaseAcrossRestart(t *testing.T) {
	s, store, p, _ := fallbackHarness(t)
	now := time.Now()
	store.mutate(t, func(state *accountRefillState) {
		state.Jobs = []AccountRefillJob{fallbackPrimaryJob("stalled", "processing", now.Add(-5*time.Minute))}
	})
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.False(t, refillMuzeStalled(&state, state.Jobs[0], now.Add(-time.Nanosecond)))
	require.True(t, refillMuzeStalled(&state, state.Jobs[0], now))
	require.NoError(t, s.run(context.Background(), true))
	require.Zero(t, p.orders, "check is read-only")
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, p.orders)
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, state.Jobs[0].ID, state.Jobs[1].MuzeFallbackJobID)
	require.Equal(t, "processing", state.Jobs[1].Status, "original is retained for reconciliation")
	require.True(t, refillJobTimedOut(state.Config, state.Jobs[1], now))
	require.Nil(t, captureRefillTimeoutNotice(&state, now), "a 5-minute backup must not announce the independent 10-minute primary timeout")
	for i := 0; i < 4; i++ {
		// A new service instance has no in-memory deduplication state.
		restarted := *s
		require.NoError(t, restarted.run(context.Background(), false))
	}
	require.Equal(t, 1, p.orders)
	store.mutate(t, func(state *accountRefillState) {
		state.Jobs[0].Status, state.Jobs[0].SupplierStatus = "failed", "rejected"
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, p.orders, "a failed backup cannot replay the same primary trigger")
}

func TestMuzeFallbackGuardsAndUnknownResults(t *testing.T) {
	for _, scenario := range []string{"off", "automation-off", "monitor-off", "push-off", "manual", "daily", "wrong-group", "other-provider", "already-imported", "not-due", "custom-timeout", "quota", "daily-limit", "healthy", "pending-backup", "no-connection", "muze-backoff"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, accounts := fallbackHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				j := fallbackPrimaryJob("original", "supplier_unknown", time.Now().Add(-6*time.Minute))
				switch scenario {
				case "off":
					state.Config.MuzeFallback.Enabled = false
				case "automation-off":
					state.Config.Enabled = false
				case "monitor-off":
					state.Config.MonitorEnabled = false
				case "push-off":
					state.Config.PushEnabled = false
				case "manual":
					j.ManualRequestID = "manual"
				case "daily":
					j.DailyBatchID = "daily"
				case "wrong-group":
					j.GroupID = 3
				case "other-provider":
					j.Provider = refillProviderMuze
					j.Status = "blocked"
				case "already-imported":
					j.AccountID = 42
				case "not-due":
					j.CreatedAt = time.Now().Add(-4 * time.Minute)
				case "custom-timeout":
					state.Config.MuzeFallback.TimeoutMinutes = 7
				case "quota":
					p.available = 0
				case "daily-limit":
					state.Usage[time.Now().In(refillTimezone).Format("2006-01-02")] = AccountRefillDailyUsage{Count: state.Config.DailyMaxCount}
				case "healthy":
					accounts.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))}
				case "pending-backup":
					backup := fallbackPrimaryJob("backup", "blocked", time.Now())
					backup.Provider, backup.FallbackForJobID = refillProviderMuze, "previous"
					state.Jobs = append(state.Jobs, backup)
				case "no-connection":
					state.Muze.EncryptedToken = ""
				case "muze-backoff":
					state.Muze.RetryAt = time.Now().Add(time.Hour)
				}
				state.Jobs = append(state.Jobs, j)
			})
			require.NoError(t, s.run(context.Background(), false))
			require.Zero(t, p.orders)
		})
	}
	// A lost primary response becomes eligible by age, never by retry count.
	s, store, p, _ := fallbackHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		j := fallbackPrimaryJob("unknown", "supplier_unknown", time.Now().Add(-6*time.Minute))
		j.Attempts = 100
		state.Jobs = []AccountRefillJob{j}
	})
	state, _ := decodeAccountRefillState(store.raw)
	require.Empty(t, refillMuzeFailures(&state))
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, p.orders)
}

func TestMuzeFallbackDurabilityAndUncertainPOST(t *testing.T) {
	for _, scenario := range []string{"reservation-save", "post-timeout", "post-save"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := fallbackHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				state.Jobs = []AccountRefillJob{fallbackPrimaryJob("stalled", "review", time.Now().Add(-6*time.Minute))}
			})
			switch scenario {
			case "reservation-save":
				store.failSave = true
			case "post-timeout":
				p.orderError = errors.New("response lost")
			case "post-save":
				p.failAfterPost = true
			}
			err := s.run(context.Background(), false)
			if scenario != "post-timeout" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if scenario == "reservation-save" {
				require.Zero(t, p.orders)
				return
			}
			store.failSave = false
			for i := 0; i < 3; i++ {
				store.mutate(t, func(state *accountRefillState) { state.Jobs[0].NextAttemptAt = time.Time{} })
				require.NoError(t, s.run(context.Background(), false))
			}
			require.Equal(t, 1, p.orders)
		})
	}
}

type fallbackProductsHook struct {
	*muzeTestProvider
	hook func()
}

func (p *fallbackProductsHook) Products(ctx context.Context, token string) ([]MuzeProduct, error) {
	p.hook()
	return p.muzeTestProvider.Products(ctx, token)
}

func TestMuzeFallbackRechecksDemandAfterQuotaLookup(t *testing.T) {
	s, store, p, accounts := fallbackHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		now := time.Now()
		state.Muze.CheckedAt = &now // Only the purchase quote invokes the hook.
		state.Jobs = []AccountRefillJob{fallbackPrimaryJob("stalled", "review", now.Add(-6*time.Minute))}
	})
	s.muze = &fallbackProductsHook{p, func() { accounts.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))} }}
	require.NoError(t, s.run(context.Background(), false))
	require.Zero(t, p.orders)
}

func TestMuzeFallbackConfigurePreservesOldClientsAndStartsFresh(t *testing.T) {
	s, store, _, _ := fallbackHarness(t)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	c := state.Config
	c.MuzeFallback = nil
	require.NoError(t, s.Configure(context.Background(), c, "rst_fixture", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.Config.MuzeFallback.Enabled)
	require.Equal(t, 3, state.Config.MuzeFallback.FailureThreshold)
	for _, invalid := range []AccountRefillMuzeFallbackConfig{{true, 0, 5}, {true, 101, 5}, {true, 3, 0}, {true, 3, 1441}} {
		c.MuzeFallback = &invalid
		require.Error(t, c.Validate())
	}
	store.mutate(t, func(state *accountRefillState) { state.Config.MuzeFallback = nil })
	state, _ = decodeAccountRefillState(store.raw)
	c = state.Config
	c.MuzeFallback = &AccountRefillMuzeFallbackConfig{true, 3, 5}
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, _ = decodeAccountRefillState(store.raw)
	require.False(t, state.MuzeFallback.Since.IsZero())
	store.mutate(t, func(state *accountRefillState) { state.Muze.EncryptedToken = "" })
	require.ErrorContains(t, s.Configure(context.Background(), c, "", state.Revision), "muze连接")
}

func TestMuzeFallbackReplacesThirdActualFailureBeforeFourthPrimaryOrder(t *testing.T) {
	s, store, p, _ := fallbackHarness(t)
	primary := s.provider.(*refillTestProvider)
	primary.status = "failed"
	store.mutate(t, func(state *accountRefillState) { state.EncryptedToken = "ciphertext" })
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	for i := 0; i < 3; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(context.Background(), false))
	}
	require.Len(t, primary.submitted, 3)
	require.Equal(t, 1, p.orders)
}

func TestMuzeFallbackSharesCrossGroupDemandAndPrepaidReservations(t *testing.T) {
	for _, scenario := range []string{"same-pool", "same-wallet"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := fallbackHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				state.Jobs = []AccountRefillJob{fallbackPrimaryJob("stalled", "review", time.Now().Add(-6*time.Minute))}
			})
			child := &refillTestStore{}
			child.mutate(t, func(state *accountRefillState) {
				state.Config = refillTestConfig()
				state.Config.GroupID, state.Config.DeliveryGroupIDs = 3, []int64{3}
				state.Muze.TokenHash = "same-wallet"
				j := fallbackPrimaryJob("other", "submitting", time.Now())
				j.Provider, j.Plan, j.SupplierID = refillProviderMuze, "pro20x", ""
				j.GroupID, j.DeliveryGroupIDs = 3, []int64{3}
				if scenario == "same-pool" {
					j.DeliveryGroupIDs = []int64{2, 3}
				} else {
					p.available = 1
				}
				state.Jobs = []AccountRefillJob{j}
			})
			s.store = &refillGroupedTestStore{store, map[int64]*refillTestStore{3: child}}
			require.NoError(t, s.run(context.Background(), false))
			require.Zero(t, p.orders)
		})
	}
}

func TestMuzeFallbackAdoptsDeliveryAndPreservesOriginalReconciliation(t *testing.T) {
	s, store, p, accounts := fallbackHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		state.Jobs = []AccountRefillJob{fallbackPrimaryJob("stalled", "review", time.Now().Add(-6*time.Minute))}
	})
	require.NoError(t, s.run(context.Background(), false))
	lease := time.Now().Add(time.Hour).Truncate(time.Second)
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email, p.cycle.RefundDueAt = true, "344", "backup@example.test", lease.Format(time.RFC3339)
	accounts.accounts = []Account{{ID: 344, CreatedAt: time.Now(), Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"email": "backup@example.test", "access_token": "fixture", "refresh_token": "refresh"}, Extra: map[string]any{}}}
	store.mutate(t, func(state *accountRefillState) { state.Jobs[0].NextAttemptAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "imported", state.Jobs[0].Status)
	require.Equal(t, 1, accounts.adopt)
	require.Zero(t, accounts.creates)
	require.Equal(t, lease.Add(10*time.Minute).Unix(), accounts.accounts[0].ExpiresAt.Unix())
	require.Equal(t, 77, accounts.accounts[0].Concurrency)
	require.Equal(t, "review", state.Jobs[1].Status)
	require.Equal(t, 1, p.orders)
	markRefillFailureReplacement(&state, &state.Jobs[1])
	require.False(t, state.Jobs[1].ReplacementPending)
	// A completed backup also resets failures that occurred while it was in flight.
	failure := fallbackPrimaryJob("new-failure", "failed", state.Jobs[0].FinishedAt.Add(-time.Second))
	state.MuzeFallback.LastAt = nil
	state.Jobs = append(state.Jobs, failure)
	require.Empty(t, refillMuzeFailures(&state))
}
