//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type muzeAlertCatalog struct {
	*muzeTestProvider
	err error
}

func (p *muzeAlertCatalog) Products(ctx context.Context, token string) ([]MuzeProduct, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.muzeTestProvider.Products(ctx, token)
}

func TestMuzeRemainingAlertFromWorker(t *testing.T) {
	s, store, supplier, _ := muzeHarness(t)
	catalog := &muzeAlertCatalog{muzeTestProvider: supplier}
	s.muze = catalog
	robot := &refillNoticeStub{}
	s.notifier = robot
	ctx := context.Background()
	c := refillTestConfig()
	c.Enabled, c.MonitorEnabled = false, false
	c.Notifications.MuzeRemaining = &MuzeRemainingNotificationConfig{Enabled: true, Threshold: 3, Products: []string{"pro20x", "pro20xv2", "future-plan"}}
	require.Error(t, s.Configure(ctx, c, "", 1), "a Muze-only alert also needs a configured channel")
	require.NoError(t, s.Configure(ctx, c, "", 1, fixtureRefillWebhook))
	sample := func(products ...MuzeProduct) {
		supplier.products = products
		store.mutate(t, func(state *accountRefillState) {
			state.Muze.CheckedAt = nil
			state.Muze.RetryAt = time.Time{}
			state.ProviderRetryAt = time.Now().Add(time.Hour)
		})
		require.NoError(t, s.run(ctx, false))
	}
	both := func(n int) []MuzeProduct {
		return []MuzeProduct{{Product: "pro20xv2", Remaining: n}, {Product: "pro20x", Remaining: n, Closed: true}, {Product: "pro5x", Remaining: 0}}
	}
	sample(both(115)...)
	require.Empty(t, robot.messages, "stock, closed status and unselected plans do not trigger a prepaid alert")
	sample(both(3)...)
	require.Len(t, robot.messages, 1, "two selected variants share one alert, including at the threshold")
	require.Contains(t, robot.messages[0], "剩余次数：3")
	require.Contains(t, robot.messages[0], "北京时间")
	sample(append(both(0), MuzeProduct{Product: "future-plan", Remaining: 0})...)
	require.Len(t, robot.messages, 2)
	require.Contains(t, robot.messages[1], "future-plan")
	catalog.err = errors.New("catalog offline")
	sample(both(8)...)
	catalog.err = nil
	sample(MuzeProduct{Product: "pro5x", Remaining: 20}) // A missing SKU cannot rearm an alert.
	sample(both(0)...)
	require.Len(t, robot.messages, 2)
	sample(both(4)...)
	sample(both(0)...)
	require.Len(t, robot.messages, 3)
	require.Contains(t, robot.messages[2], "剩余次数：0")
	require.Zero(t, supplier.orders)
}

func TestMuzeRemainingAlertSettingsSurviveLegacySave(t *testing.T) {
	s, store, _, _ := muzeHarness(t)
	ctx := context.Background()
	c := refillTestConfig()
	c.Enabled, c.MonitorEnabled = false, false
	c.Notifications.MuzeRemaining = &MuzeRemainingNotificationConfig{Enabled: true, Threshold: 0, Products: []string{"pro20xv2", "pro5x"}}
	require.NoError(t, s.Configure(ctx, c, "", 1, fixtureRefillWebhook))
	store.mutate(t, func(st *accountRefillState) {
		st.MuzeRemainingAlerted = map[string]bool{"pro20x": true, "pro5x": true}
		st.LowBalanceAlerted = true
	})
	c.Notifications.MuzeRemaining = nil
	require.NoError(t, s.Configure(ctx, c, "", 2))
	view, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"pro20xv2", "pro5x"}, view.Config.Notifications.MuzeRemaining.Products)
	c = view.Config
	c.Notifications.MuzeRemaining.Products = []string{"pro20xv2"}
	require.NoError(t, s.Configure(ctx, c, "", 3))
	st, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"pro20x": true}, st.MuzeRemainingAlerted)
	require.True(t, st.LowBalanceAlerted)
	c.Notifications.MuzeRemaining.Enabled = false
	require.NoError(t, s.Configure(ctx, c, "", 4))
	st, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, st.MuzeRemainingAlerted)
	for _, invalid := range []MuzeRemainingNotificationConfig{{Enabled: true}, {Threshold: -1}, {Threshold: 10_000_001}, {Products: []string{"bad id"}}, {Products: []string{"x", "x"}}} {
		require.Error(t, invalid.validate())
	}
}

func TestMuzeRemainingAlertRequiresFreshConfirmedCatalogAndDurableReservation(t *testing.T) {
	s, store, _, _ := muzeHarness(t)
	robot := &refillNoticeStub{}
	s.notifier = robot
	st, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	st.EncryptedWebhook = "ciphertext"
	st.Config.Notifications.MuzeRemaining = &MuzeRemainingNotificationConfig{Enabled: true, Products: []string{"pro20xv2"}}
	now := time.Now()
	st.Muze.Products = []MuzeProduct{{Product: "pro20xv2", Remaining: 0}}
	notify := func(persist func() error) error {
		return s.notifyMuzeRemaining(context.Background(), &st, now, persist)
	}
	persist := func() error { return nil }
	stale := now.Add(-muzeCatalogFreshness)
	st.Muze.CheckedAt = &stale
	require.NoError(t, notify(persist))
	st.Muze.CheckedAt = &now
	st.Muze.RetryAt = now.Add(time.Minute)
	require.NoError(t, notify(persist))
	st.Muze.RetryAt = time.Time{}
	require.Error(t, notify(func() error { return errors.New("write failed") }))
	require.Empty(t, robot.messages)
}
