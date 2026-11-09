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

type fenduduRecoveryProvider struct {
	*fenduduTestProvider
	pages     map[string]*fenduduCyclePage
	reads     int
	readError error
}

func (p *fenduduRecoveryProvider) Cycles(_ context.Context, _ string, cursor string) (*fenduduCyclePage, error) {
	p.reads++
	return p.pages[cursor], p.readError
}

func pushFenduduFixture(p *fenduduTestProvider, accounts *fenduduTestAdmin) time.Time {
	created := time.Now()
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email = true, "344", "fixture@example.test"
	accounts.accounts = []Account{{ID: 344, Name: "supplier", CreatedAt: created, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Credentials: map[string]any{"email": p.cycle.Email, "access_token": "fixture"}, Extra: map[string]any{}}}
	return created
}

func TestFenduduLostReceiptRecoversWithoutAnotherPurchase(t *testing.T) {
	for _, mode := range []string{"lost-response", "crash-after-post", "already-closed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Manual request IDs require a 13-digit Unix millisecond timestamp.
				time.Sleep(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Now()))
				s, store, provider, accounts := fenduduHarness(t)
				p := &fenduduRecoveryProvider{fenduduTestProvider: provider, pages: map[string]*fenduduCyclePage{}}
				s.fendudu = p
				provider.orderError = errors.New("lost response")
				provider.failAfterPost = mode == "crash-after-post"
				ctx := context.Background()
				require.NoError(t, s.Manual(ctx, fenduduRequest()))
				err := s.run(ctx, false)
				if provider.failAfterPost {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				store.failSave = false
				time.Sleep(2 * time.Minute)
				created := pushFenduduFixture(provider, accounts)
				if mode == "already-closed" {
					provider.cycle.Status = "closed"
					manualExpiry := created.Add(90 * time.Minute)
					accounts.accounts[0].ExpiresAt = &manualExpiry
					time.Sleep(2 * time.Hour)
				}
				p.pages[""] = &fenduduCyclePage{Success: true, Items: []fenduduCycle{*provider.cycle}}
				// Simulate process restart with only persisted state and the account.
				restarted := *s
				s = &restarted
				refillDue(t, store)
				require.NoError(t, s.run(ctx, false))
				v, err := s.View(ctx)
				require.NoError(t, err)
				require.Equal(t, "rvc_fixture", v.Jobs[0].SupplierID)
				require.EqualValues(t, 344, v.Jobs[0].AccountID)
				require.Equal(t, created.Add(100*time.Minute).Unix(), *v.Jobs[0].ExpiresAt)
				require.Equal(t, created.Add(100*time.Minute).Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
				require.Equal(t, 1, provider.orders)
				require.Zero(t, accounts.creates)
				if mode == "already-closed" {
					require.Equal(t, "delivery_expired", v.Jobs[0].Status)
					require.False(t, accounts.accounts[0].Schedulable)
					require.Equal(t, "inactive", accounts.accounts[0].Status)
					require.Equal(t, created.Add(90*time.Minute).Unix(), accounts.accounts[0].ExpiresAt.Unix())
					require.Zero(t, accounts.adopt)
					require.Equal(t, 1, accounts.closed)
				} else {
					require.Equal(t, "imported", v.Jobs[0].Status)
					require.Equal(t, 1, accounts.adopt)
					require.Equal(t, created.Add(time.Duration(100+*v.Jobs[0].ExpiryGraceMinutes)*time.Minute).Unix(), accounts.accounts[0].ExpiresAt.Unix())
					time.Sleep(10 * time.Minute)
					provider.cycle.RevokeAt = time.Now().Add(8 * time.Hour).Format(time.RFC3339)
					require.NoError(t, s.Check(ctx))
					require.Equal(t, time.Now().Add(8*time.Hour).Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey], "a subsequently available supplier expiry replaces the fallback")
				}
			})
		})
	}
}

func TestFenduduRecoveryRejectsUncertainMatches(t *testing.T) {
	for _, mode := range []string{"ambiguous", "pending-competitor", "wrong-email", "wrong-connection", "old-order", "old-account", "claimed", "competing-local", "competing-group", "repeated-cursor", "incomplete-pages", "malformed-list", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			s, store, provider, accounts := fenduduHarness(t)
			provider.orderError = errors.New("lost response")
			ctx := context.Background()
			require.NoError(t, s.Manual(ctx, fenduduRequest()))
			require.NoError(t, s.run(ctx, false))
			pushFenduduFixture(provider, accounts)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			cycle := *provider.cycle
			p := &fenduduRecoveryProvider{fenduduTestProvider: provider, pages: map[string]*fenduduCyclePage{
				"": {Success: true, Items: []fenduduCycle{cycle}},
			}}
			s.fendudu = p
			switch mode {
			case "ambiguous", "pending-competitor":
				second := cycle
				second.ID = "rvc_other"
				if mode == "pending-competitor" {
					second.Pushed, second.PushProvider, second.PushConfigID = false, "", ""
				}
				p.pages[""].Items = append(p.pages[""].Items, second)
			case "wrong-email":
				accounts.accounts[0].Credentials["email"] = "other@example.test"
			case "wrong-connection":
				p.pages[""].Items[0].PushConfigID = "s2e_other"
			case "old-order":
				p.pages[""].Items[0].CreatedAt = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
			case "old-account":
				accounts.accounts[0].CreatedAt = time.Now().Add(-time.Hour)
			case "claimed", "competing-local", "competing-group":
				other := state.Jobs[0]
				other.ID = "another-local-job"
				if mode == "claimed" {
					other.SupplierID = cycle.ID
				}
				if mode == "competing-group" {
					child := &refillTestStore{raw: store.raw}
					child.mutate(t, func(v *accountRefillState) { v.Config.GroupID = 3; v.Jobs = []AccountRefillJob{other} })
					s.store = &refillGroupedTestStore{refillTestStore: store, children: map[int64]*refillTestStore{3: child}}
				} else {
					state.Jobs = append(state.Jobs, other)
				}
			case "repeated-cursor", "incomplete-pages":
				p.pages[""].HasMore, p.pages[""].NextCursor = true, "next"
				if mode == "repeated-cursor" {
					p.pages["next"] = &fenduduCyclePage{Success: true, Items: []fenduduCycle{cycle}, HasMore: true, NextCursor: "next"}
				}
			case "malformed-list":
				p.pages[""].Items = nil
			case "read-error":
				p.readError = errors.New("temporary read error")
			}
			_, err = s.recoverFenduduOrder(ctx, "fixture", &state, state.Jobs[0])
			require.Error(t, err)
			require.Zero(t, accounts.adopt)
			require.Zero(t, accounts.closed)
			require.Equal(t, 1, provider.orders)
			require.Empty(t, state.Jobs[0].SupplierID)
		})
	}
}

func TestFenduduRecoveryTraversesPagesBeforeAdopting(t *testing.T) {
	s, store, provider, accounts := fenduduHarness(t)
	provider.orderError = errors.New("lost response")
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	pushFenduduFixture(provider, accounts)
	newer := *provider.cycle
	newer.ID, newer.CreatedAt = "unrelated", time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	p := &fenduduRecoveryProvider{fenduduTestProvider: provider, pages: map[string]*fenduduCyclePage{
		"":      {Success: true, Items: []fenduduCycle{newer}, HasMore: true, NextCursor: "page2"},
		"page2": {Success: true, Items: []fenduduCycle{*provider.cycle}},
	}}
	s.fendudu = p
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 2, p.reads)
	require.Equal(t, 1, accounts.adopt)
	require.Equal(t, 1, provider.orders)
}

func TestFenduduFixedCutoffKeepsSeparateHundredMinuteLease(t *testing.T) {
	s, store, p, accounts := fenduduHarness(t)
	ctx := context.Background()
	fixed := time.Now().Add(40 * time.Minute).Unix()
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Delivery.ExpiryMode, v.Config.Delivery.FixedExpiresAt = "fixed", &fixed
		v.Config.ImportExpiryGraceMinutes = 10
	})
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	created := pushFenduduFixture(p, accounts)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, fixed+600, accounts.accounts[0].ExpiresAt.Unix())
	require.Equal(t, created.Add(100*time.Minute).Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
}
