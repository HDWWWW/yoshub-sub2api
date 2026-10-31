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

const astraPriorityTestGroupID int64 = 58200

func astraPriorityTestAccounts() []Account {
	accounts := newLegacySchedulerDecisionTestAccounts(astraPriorityTestGroupID, true)
	soon, later := time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)
	accounts[0].Priority, accounts[1].Priority = 1, 2
	accounts[0].ExpiresAt, accounts[1].ExpiresAt = &soon, &later
	accounts[0].Concurrency, accounts[1].Concurrency = 10, 10
	return accounts
}

func newAstraPriorityTestService(accounts []Account, advanced, weighted, loadBatch bool, concurrency schedulerTestConcurrencyCache) *OpenAIGatewayService {
	svc := newLegacySchedulerDecisionTestService(accounts, loadBatch, concurrency)
	svc.cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled = true
	svc.cfg.Gateway.OpenAIWS.LBTopK = 1
	// Deliberately favor the later account by score. Priority must be a tier,
	// not a larger weight that another preference can outweigh.
	svc.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1000
	svc.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky = 1000
	svc.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.PreviousResponse = 1000
	svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService(fmt.Sprint(advanced), fmt.Sprint(weighted))
	return svc
}

func selectAstraPriorityTestAccount(t *testing.T, svc *OpenAIGatewayService, model, previous string, movable bool, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision) {
	t.Helper()
	groupID := astraPriorityTestGroupID
	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), &groupID, previous, "astra-priority-session", model, excluded,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, movable, true)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		t.Cleanup(selection.ReleaseFunc)
	}
	return selection, decision
}

func bindAstraPriorityTestSession(t *testing.T, svc *OpenAIGatewayService, accountID int64) {
	t.Helper()
	groupID := astraPriorityTestGroupID
	require.NoError(t, svc.setStickySessionAccountID(context.Background(), &groupID, "astra-priority-session", accountID, time.Hour))
}

func TestAstraPriorityOverridesScoreAndSessionAcrossSchedulerModes(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, weighted := range []bool{false, true} {
			for _, loadBatch := range []bool{false, true} {
				t.Run(fmt.Sprintf("advanced=%v/weighted=%v/batch=%v", advanced, weighted, loadBatch), func(t *testing.T) {
					accounts := astraPriorityTestAccounts()
					svc := newAstraPriorityTestService(accounts, advanced, weighted, loadBatch, schedulerTestConcurrencyCache{
						loadMap: map[int64]*AccountLoadInfo{
							accounts[0].ID: {AccountID: accounts[0].ID, LoadRate: 90, CurrentConcurrency: 9},
						},
					})
					bindAstraPriorityTestSession(t, svc, accounts[1].ID)
					selection, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
					require.Equal(t, accounts[0].ID, selection.Account.ID)
					require.True(t, selection.Acquired)
					require.False(t, decision.StickySessionHit)
					require.Equal(t, openAIAccountScheduleLayerPriorityFirst, decision.Layer)
					if advanced {
						require.Equal(t, int64(1), svc.SnapshotOpenAIAccountSchedulerMetrics().SelectTotal)
						require.Equal(t, int64(1), svc.SnapshotOpenAIAccountSchedulerMetrics().LoadBalanceSelectTotal)
					}
				})
			}
		}
	}
}

func TestAstraPriorityFallsThroughUnavailableAccounts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Account, *schedulerTestConcurrencyCache)
	}{
		{"disabled", func(a *Account, _ *schedulerTestConcurrencyCache) { a.Schedulable = false }},
		{"expired", func(a *Account, _ *schedulerTestConcurrencyCache) {
			past := time.Now().Add(-time.Minute)
			a.ExpiresAt, a.AutoPauseOnExpired = &past, true
		}},
		{"rate_limited", func(a *Account, _ *schedulerTestConcurrencyCache) {
			future := time.Now().Add(time.Minute)
			a.RateLimitResetAt = &future
		}},
		{"quota_exhausted", func(a *Account, _ *schedulerTestConcurrencyCache) {
			a.Extra = map[string]any{"codex_5h_used_percent": 100.0, "auto_pause_5h_threshold": 0.95}
		}},
		{"model_unsupported", func(a *Account, _ *schedulerTestConcurrencyCache) {
			a.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.1": "gpt-5.1"}}
		}},
		{"concurrency_full", func(a *Account, c *schedulerTestConcurrencyCache) {
			c.loadMap = map[int64]*AccountLoadInfo{a.ID: {AccountID: a.ID, LoadRate: 100, CurrentConcurrency: a.Concurrency}}
		}},
		{"slot_race", func(a *Account, c *schedulerTestConcurrencyCache) {
			c.acquireResults = map[int64]bool{a.ID: false}
		}},
		{"load_cache_failure_and_busy_slot", func(a *Account, c *schedulerTestConcurrencyCache) {
			c.loadBatchErr = errors.New("load cache unavailable")
			c.acquireResults = map[int64]bool{a.ID: false}
		}},
	}
	for _, advanced := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("advanced=%v/%s", advanced, tc.name), func(t *testing.T) {
				accounts := astraPriorityTestAccounts()
				cache := schedulerTestConcurrencyCache{}
				tc.mutate(&accounts[0], &cache)
				svc := newAstraPriorityTestService(accounts, advanced, true, false, cache)
				bindAstraPriorityTestSession(t, svc, accounts[0].ID)
				selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
				require.Equal(t, accounts[1].ID, selection.Account.ID)
				require.True(t, selection.Acquired, "try the next account before waiting on a busy account")
			})
		}
	}
}

func TestAstraPriorityRetryExclusions(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, true, true, true, schedulerTestConcurrencyCache{})
	selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, map[int64]struct{}{accounts[0].ID: {}})
	require.Equal(t, accounts[1].ID, selection.Account.ID)
}

func TestAstraPriorityExhaustsSameTierBeyondTopK(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	third := accounts[0]
	third.ID = 38103
	accounts = append(accounts, third)
	var attempted []int64
	svc := newAstraPriorityTestService(accounts, true, true, true, schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{accounts[0].ID: false},
		acquiredIDs:    &attempted,
	})
	bindAstraPriorityTestSession(t, svc, accounts[1].ID)
	selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
	require.Equal(t, third.ID, selection.Account.ID)
	require.Equal(t, []int64{accounts[0].ID, third.ID}, attempted)
}

func TestAstraPriorityWaitsOnEarliestTierWhenAllBusy(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, true, true, true, schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{accounts[0].ID: false, accounts[1].ID: false},
	})
	bindAstraPriorityTestSession(t, svc, accounts[1].ID)
	selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
	require.Equal(t, accounts[0].ID, selection.Account.ID)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, accounts[0].ID, selection.WaitPlan.AccountID)
}

func TestAstraPriorityKeepsSoftAffinityWithinSameTier(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	accounts[1].Priority = accounts[0].Priority
	svc := newAstraPriorityTestService(accounts, false, false, false, schedulerTestConcurrencyCache{})
	bindAstraPriorityTestSession(t, svc, accounts[1].ID)
	selection, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
	require.Equal(t, accounts[1].ID, selection.Account.ID)
	require.True(t, decision.StickySessionHit)
}

func TestAstraPriorityPreservesRequiredPreviousResponseOwner(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, movable := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%v/movable=%v", advanced, movable), func(t *testing.T) {
				accounts := astraPriorityTestAccounts()
				svc := newAstraPriorityTestService(accounts, advanced, true, true, schedulerTestConcurrencyCache{})
				require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(context.Background(), astraPriorityTestGroupID, "resp_astra_priority", accounts[1].ID, time.Hour))
				selection, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "resp_astra_priority", movable, nil)
				want := accounts[1].ID
				if movable {
					want = accounts[0].ID
				}
				require.Equal(t, want, selection.Account.ID)
				require.Equal(t, !movable, decision.StickyPreviousHit)
			})
		}
	}
}

func TestAstraPrioritySubscriptionPreferenceStaysWithinTier(t *testing.T) {
	for _, sameTier := range []bool{false, true} {
		t.Run(fmt.Sprint(sameTier), func(t *testing.T) {
			accounts := astraPriorityTestAccounts()
			accounts[1].Type = AccountTypeOAuth
			accounts[1].Credentials = map[string]any{"plan_type": "plus"}
			want := accounts[0].ID
			if sameTier {
				accounts[1].Priority = accounts[0].Priority
				want = accounts[1].ID
			}
			svc := newAstraPriorityTestService(accounts, true, true, true, schedulerTestConcurrencyCache{})
			svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "true", "true")
			selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
			require.Equal(t, want, selection.Account.ID)
		})
	}
}

func TestAstraPriorityScopeAndRollback(t *testing.T) {
	for _, tc := range []struct {
		model   string
		enabled bool
		want    int
	}{
		{"gpt-6-astra", true, 0},
		{"gpt-6", true, 0},
		{"openai/gpt-6-astra", true, 0},
		{"gpt-6-astra-2026-09-01", true, 0},
		{"gpt-6-astra", false, 1},
		{"gpt-6-sol", true, 1},
		{"gpt-6.1-sol", true, 1},
		{"gpt-5.1", true, 1},
	} {
		t.Run(fmt.Sprintf("%s/enabled=%v", tc.model, tc.enabled), func(t *testing.T) {
			accounts := astraPriorityTestAccounts()
			svc := newAstraPriorityTestService(accounts, false, false, true, schedulerTestConcurrencyCache{})
			svc.cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled = tc.enabled
			bindAstraPriorityTestSession(t, svc, accounts[1].ID)
			selection, _ := selectAstraPriorityTestAccount(t, svc, tc.model, "", false, nil)
			require.Equal(t, accounts[tc.want].ID, selection.Account.ID)
			require.False(t, svc.preferOpenAIAstraPriority(PlatformGrok, tc.model))
		})
	}
}
