//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func reversePriorityTestContext(enabled bool) context.Context {
	return context.WithValue(context.Background(), ctxkey.Group, &Group{
		ID: astraPriorityTestGroupID, Platform: PlatformOpenAI, Status: StatusActive,
		Hydrated: true, ReverseAccountPriority: enabled,
	})
}

func selectReversePriorityTestAccount(t *testing.T, svc *OpenAIGatewayService, ctx context.Context, previous string, movable bool) (*AccountSelectionResult, OpenAIAccountScheduleDecision) {
	t.Helper()
	groupID := astraPriorityTestGroupID
	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx, &groupID, previous, "astra-priority-session", "gpt-5.1", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, movable, true,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		t.Cleanup(selection.ReleaseFunc)
	}
	return selection, decision
}

func TestReversePriorityOverridesScoreAndOrdinaryStickyAcrossSchedulerModes(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, weighted := range []bool{false, true} {
			for _, loadBatch := range []bool{false, true} {
				t.Run(fmt.Sprintf("advanced=%v/weighted=%v/batch=%v", advanced, weighted, loadBatch), func(t *testing.T) {
					accounts := astraPriorityTestAccounts()
					svc := newAstraPriorityTestService(accounts, advanced, weighted, loadBatch, schedulerTestConcurrencyCache{
						loadMap: map[int64]*AccountLoadInfo{
							accounts[1].ID: {AccountID: accounts[1].ID, LoadRate: 90, CurrentConcurrency: 9},
						},
					})
					bindAstraPriorityTestSession(t, svc, accounts[0].ID)
					selection, decision := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
					require.Equal(t, accounts[1].ID, selection.Account.ID)
					require.True(t, selection.Acquired)
					require.False(t, decision.StickySessionHit)
					require.Equal(t, openAIAccountScheduleLayerPriorityFirst, decision.Layer)
				})
			}
		}
	}
}

func TestReversePriorityFallsThroughUnavailableHigherTier(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Account, *schedulerTestConcurrencyCache)
	}{
		{"disabled", func(a *Account, _ *schedulerTestConcurrencyCache) { a.Schedulable = false }},
		{"quota_exhausted", func(a *Account, _ *schedulerTestConcurrencyCache) {
			a.Extra = map[string]any{"codex_5h_used_percent": 100.0, "auto_pause_5h_threshold": 0.95}
		}},
		{"concurrency_full", func(a *Account, cache *schedulerTestConcurrencyCache) {
			cache.loadMap = map[int64]*AccountLoadInfo{a.ID: {AccountID: a.ID, LoadRate: 100, CurrentConcurrency: a.Concurrency}}
		}},
		{"slot_race", func(a *Account, cache *schedulerTestConcurrencyCache) {
			cache.acquireResults = map[int64]bool{a.ID: false}
		}},
		{"load_cache_failure_and_busy_slot", func(a *Account, cache *schedulerTestConcurrencyCache) {
			cache.loadBatchErr = errors.New("load cache unavailable")
			cache.acquireResults = map[int64]bool{a.ID: false}
		}},
	}
	for _, advanced := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("advanced=%v/%s", advanced, tc.name), func(t *testing.T) {
				accounts := astraPriorityTestAccounts()
				cache := schedulerTestConcurrencyCache{}
				tc.mutate(&accounts[1], &cache)
				svc := newAstraPriorityTestService(accounts, advanced, true, false, cache)
				selection, _ := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
				require.Equal(t, accounts[0].ID, selection.Account.ID)
				require.True(t, selection.Acquired)
			})
		}
	}
}

func TestReversePriorityExhaustsSameTierBeyondTopK(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	third := accounts[1]
	third.ID = 38103
	accounts = append(accounts, third)
	var attempted []int64
	svc := newAstraPriorityTestService(accounts, true, true, true, schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{accounts[1].ID: false}, acquiredIDs: &attempted,
	})
	bindAstraPriorityTestSession(t, svc, accounts[0].ID)
	selection, _ := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
	require.Equal(t, third.ID, selection.Account.ID)
	require.Equal(t, []int64{accounts[1].ID, third.ID}, attempted)
}

func TestReversePriorityConsumesCapacityFromFourThroughOne(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	for priority := 3; priority <= 4; priority++ {
		account := accounts[0]
		account.ID += int64(priority - 1)
		account.Priority = priority
		accounts = append(accounts, account)
	}
	for available := 4; available >= 1; available-- {
		t.Run(fmt.Sprintf("highest_available=%d", available), func(t *testing.T) {
			loadMap := map[int64]*AccountLoadInfo{}
			for _, account := range accounts {
				if account.Priority > available {
					loadMap[account.ID] = &AccountLoadInfo{AccountID: account.ID, LoadRate: 100, CurrentConcurrency: account.Concurrency}
				}
			}
			svc := newAstraPriorityTestService(accounts, false, false, true, schedulerTestConcurrencyCache{loadMap: loadMap})
			selection, _ := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
			require.Equal(t, available, selection.Account.Priority)
			require.True(t, selection.Acquired)
		})
	}
}

func TestReversePriorityKeepsLoadOrderWithinSameTier(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	accounts[0].Priority = accounts[1].Priority
	svc := newAstraPriorityTestService(accounts, false, false, true, schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			accounts[1].ID: {AccountID: accounts[1].ID, LoadRate: 90, CurrentConcurrency: 9},
		},
	})
	selection, _ := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
	require.Equal(t, accounts[0].ID, selection.Account.ID)
}

func TestReversePriorityPreservesRequiredPreviousResponseOwner(t *testing.T) {
	for _, movable := range []bool{false, true} {
		t.Run(fmt.Sprint(movable), func(t *testing.T) {
			accounts := astraPriorityTestAccounts()
			svc := newAstraPriorityTestService(accounts, false, true, true, schedulerTestConcurrencyCache{})
			require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(context.Background(), astraPriorityTestGroupID, "resp_reverse_priority", accounts[0].ID, time.Hour))
			selection, decision := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "resp_reverse_priority", movable)
			want := accounts[0].ID
			if movable {
				want = accounts[1].ID
			}
			require.Equal(t, want, selection.Account.ID)
			require.Equal(t, !movable, decision.StickyPreviousHit)
		})
	}
}

func TestReversePriorityPreservesGuardianParentOwner(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, false, true, true, schedulerTestConcurrencyCache{})
	groupID := astraPriorityTestGroupID
	parentSession := "reverse-parent-session"
	ctx := context.WithValue(reversePriorityTestContext(true), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: parentSession})
	require.NoError(t, svc.setStickySessionAccountID(ctx, &groupID, parentSession, accounts[0].ID, time.Hour))
	selection, decision := selectReversePriorityTestAccount(t, svc, ctx, "", false)
	require.Equal(t, accounts[0].ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerGuardianParent, decision.Layer)
}

func TestReversePriorityScopeCompositeAndRollback(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, false, false, true, schedulerTestConcurrencyCache{})
	bindAstraPriorityTestSession(t, svc, accounts[0].ID)
	selection, _ := selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(true), "", false)
	require.Equal(t, accounts[1].ID, selection.Account.ID)
	bindAstraPriorityTestSession(t, svc, accounts[0].ID)
	selection, _ = selectReversePriorityTestAccount(t, svc, reversePriorityTestContext(false), "", false)
	require.Equal(t, accounts[0].ID, selection.Account.ID)
	groupID := astraPriorityTestGroupID
	for _, tc := range []struct {
		name     string
		group    *Group
		platform string
		want     bool
	}{
		{"same_group", &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ReverseAccountPriority: true}, PlatformOpenAI, true},
		{"other_group", &Group{ID: groupID + 1, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ReverseAccountPriority: true}, PlatformOpenAI, false},
		{"partial_group", &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, ReverseAccountPriority: true}, PlatformOpenAI, false},
		{"composite_original_group", &Group{ID: groupID + 1, Platform: PlatformComposite, Status: StatusActive, Hydrated: true, ReverseAccountPriority: true}, PlatformOpenAI, true},
		{"composite_other_provider", &Group{ID: groupID, Platform: PlatformComposite, Status: StatusActive, Hydrated: true, ReverseAccountPriority: true}, PlatformGrok, false},
		{"unsupported_group", &Group{ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true, ReverseAccountPriority: true}, PlatformOpenAI, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), ctxkey.Group, tc.group)
			require.Equal(t, tc.want, svc.openAIGroupReversesAccountPriority(ctx, &groupID, tc.platform))
		})
	}
}

func TestReversePriorityRepositoryFallbackAndCompositeOverride(t *testing.T) {
	groupID := astraPriorityTestGroupID
	svc := &OpenAIGatewayService{schedulerSnapshot: &SchedulerSnapshotService{
		groupRepo: &groupRepoStubForAdmin{getByID: &Group{ID: groupID, Platform: PlatformOpenAI, ReverseAccountPriority: true}},
	}}
	require.True(t, svc.openAIGroupReversesAccountPriority(context.Background(), &groupID, PlatformOpenAI))
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{
		ID: groupID + 1, Platform: PlatformComposite, Status: StatusActive, Hydrated: true,
	})
	require.False(t, svc.openAIGroupReversesAccountPriority(ctx, &groupID, PlatformOpenAI), "the original composite group owns policy, not the supporting group")
}

func TestReversePriorityLegacyPublicEntry(t *testing.T) {
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, false, false, false, schedulerTestConcurrencyCache{})
	groupID := astraPriorityTestGroupID
	selection, err := svc.SelectAccountWithLoadAwareness(reversePriorityTestContext(true), &groupID, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	t.Cleanup(selection.ReleaseFunc)
	require.Equal(t, accounts[1].ID, selection.Account.ID)
}
