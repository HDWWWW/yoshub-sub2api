//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fenduduExpiryAccounts struct{ *refillTestAccounts }

func (a *fenduduExpiryAccounts) UpdateExtra(_ context.Context, id int64, values map[string]any) error {
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			ApplyAccountExpiryExtra(&a.accounts[i], values)
			return nil
		}
	}
	return ErrAccountNotFound
}

func TestFenduduNativeExpiryOverridesHundredMinuteFallback(t *testing.T) {
	for _, mode := range []string{"revoke", "deadline", "invalid-revoke", "both-missing", "both-invalid", "expired-revoke"} {
		t.Run(mode, func(t *testing.T) {
			s, store, p, accounts := fenduduHarness(t)
			store.mutate(t, func(v *accountRefillState) { v.Config.ImportExpiryGraceMinutes = 10 })
			ctx := context.Background()
			require.NoError(t, s.Manual(ctx, fenduduRequest()))
			require.NoError(t, s.run(ctx, false))
			created := pushFenduduFixture(p, accounts)
			want := created.Add(240 * time.Minute).Unix()
			p.cycle.RevokeAt = time.Unix(want, 0).Format(time.RFC3339)
			p.cycle.DeadlineAt = time.Unix(want+60, 0).Format(time.RFC3339)
			switch mode {
			case "deadline", "invalid-revoke":
				p.cycle.RevokeAt = ""
				if mode == "invalid-revoke" {
					p.cycle.RevokeAt = "invalid"
				}
				want += 60
			case "both-missing", "both-invalid":
				p.cycle.RevokeAt, p.cycle.DeadlineAt = "", ""
				if mode == "both-invalid" {
					p.cycle.RevokeAt, p.cycle.DeadlineAt = "invalid", "invalid"
				}
				want = created.Add(100 * time.Minute).Unix()
			case "expired-revoke":
				want = created.Add(-time.Minute).Unix()
				p.cycle.RevokeAt = time.Unix(want, 0).Format(time.RFC3339)
			}
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			v, err := s.View(ctx)
			require.NoError(t, err)
			require.Equal(t, want, *v.Jobs[0].ExpiresAt)
			require.Equal(t, want, accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
			require.Equal(t, want+600, accounts.accounts[0].ExpiresAt.Unix())
			if mode == "expired-revoke" {
				require.Equal(t, "delivery_expired", v.Jobs[0].Status)
				require.False(t, accounts.accounts[0].Schedulable)
			} else {
				require.Equal(t, "imported", v.Jobs[0].Status)
			}
			require.Equal(t, 1, p.orders)
		})
	}
}

func TestFenduduNativeLeaseSyncRespectsCutoffManualExpiryAndRetention(t *testing.T) {
	for _, mode := range []string{"rental", "fixed", "manual", "new-retention", "expired-delivery"} {
		t.Run(mode, func(t *testing.T) {
			s, store, p, accounts := fenduduHarness(t)
			s.accounts = &fenduduExpiryAccounts{accounts.refillTestAccounts}
			fixed := time.Now().Add(45 * time.Minute).Unix()
			store.mutate(t, func(v *accountRefillState) {
				v.Config.ImportExpiryGraceMinutes = 10
				if mode == "fixed" {
					v.Config.Delivery.ExpiryMode, v.Config.Delivery.FixedExpiresAt = "fixed", &fixed
				}
			})
			ctx := context.Background()
			require.NoError(t, s.Manual(ctx, fenduduRequest()))
			require.NoError(t, s.run(ctx, false))
			created := pushFenduduFixture(p, accounts)
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			a := &accounts.accounts[0]
			if mode == "manual" || mode == "expired-delivery" {
				ApplyAccountExpiryWrite(ctx, a, fixed)
			}
			if mode == "new-retention" {
				ApplyAccountExpiryExtra(a, map[string]any{AccountImportExpiryGraceExtraKey: 1800})
			}
			if mode == "expired-delivery" {
				store.mutate(t, func(v *accountRefillState) { v.Jobs[0].Status = "delivery_expired" })
				a.Status, a.Schedulable = "inactive", false
				p.cycle.Status = "closed"
			}
			for _, phase := range []string{"native-later", "missing-again", "native-earlier"} {
				want := created.Add(180 * time.Minute).Unix()
				if phase == "native-earlier" {
					want = created.Add(30 * time.Minute).Unix()
				}
				p.cycle.RevokeAt = time.Unix(want, 0).Format(time.RFC3339)
				if phase == "missing-again" {
					p.cycle.RevokeAt = ""
				}
				require.NoError(t, s.Check(ctx))
				require.Equal(t, want, a.Extra[AccountSourceExpiryExtraKey])
				deadline := want + 600
				switch mode {
				case "manual", "expired-delivery":
					deadline = fixed
				case "fixed":
					deadline = min(want, fixed) + 600
				case "new-retention":
					deadline = want + 1800
				}
				require.Equal(t, deadline, a.ExpiresAt.Unix(), mode+"/"+phase)
			}
			if mode == "expired-delivery" {
				require.Equal(t, "inactive", a.Status)
				require.False(t, a.Schedulable)
			}
			require.Equal(t, 1, p.orders)
		})
	}
}
