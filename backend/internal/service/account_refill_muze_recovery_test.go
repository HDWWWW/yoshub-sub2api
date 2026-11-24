//go:build unit

package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type muzeRecoveryProvider struct {
	*muzeTestProvider
	found     *muzeCycle
	readError error
}

func (p *muzeRecoveryProvider) FindOrder(_ context.Context, _ string, j AccountRefillJob) (*muzeCycle, error) {
	if p.readError != nil {
		return nil, p.readError
	}
	if p.found == nil || p.found.RequestID != j.ID {
		return nil, errors.New("no matching request")
	}
	return p.found, nil
}

func pushMuzeFixture(p *muzeTestProvider, accounts *muzeTestAdmin) time.Time {
	created := time.Now()
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email = true, "344", "fixture@example.test"
	accounts.accounts = []Account{{ID: 344, Name: "supplier", CreatedAt: created, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Credentials: map[string]any{"email": p.cycle.Email, "access_token": "fixture"}, Extra: map[string]any{}}}
	return created
}

func TestMuzeLostReceiptReconcilesExactRequest(t *testing.T) {
	for _, crash := range []bool{false, true} {
		s, store, p, accounts := muzeHarness(t)
		ctx := context.Background()
		p.orderError = errors.New("lost receipt")
		p.failAfterPost = crash
		require.NoError(t, s.Manual(ctx, muzeRequest()))
		err := s.run(ctx, false)
		if crash {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		store.failSave = false
		pushMuzeFixture(p, accounts)
		p.cycle.RefundDueAt = time.Now().Add(time.Hour).Format(time.RFC3339)
		other := *p.cycle
		other.RequestID = "different"
		r := &muzeRecoveryProvider{muzeTestProvider: p, found: &other}
		s.muze = r
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
		require.Zero(t, accounts.adopt)
		r.found = p.cycle
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
		require.Equal(t, 1, accounts.adopt)
		require.Equal(t, 1, p.orders)
		v, err := s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, p.cycle.ID, v.Jobs[0].SupplierID)
		require.Equal(t, p.cycle.expiresAt(), v.Jobs[0].SupplierExpiresAt)
	}
}
