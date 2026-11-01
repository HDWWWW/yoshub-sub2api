//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type astraPriorityWriteFailureRepo struct {
	*panelRateLimitSettingRepo
}

func (r *astraPriorityWriteFailureRepo) Set(context.Context, string, string) error {
	return errors.New("write unavailable")
}

func TestAstraPriorityRuntimeDefaultsPersistenceAndReplicaRefresh(t *testing.T) {
	ctx := context.Background()
	for _, configured := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled = configured
		repo := &panelRateLimitSettingRepo{}
		first, second := NewSettingService(repo, cfg), NewSettingService(repo, cfg)
		require.Equal(t, configured, first.astraPriorityEnabled(ctx))
		require.Equal(t, configured, second.astraPriorityEnabled(ctx))
		require.NoError(t, first.SetAstraPrioritySettings(ctx, !configured))
		require.Equal(t, !configured, first.astraPriorityEnabled(ctx), "same instance applies immediately")
		// A different instance observes the saved override after its bounded cache TTL.
		second.astraPriorityCache.Store(&cachedAstraPrioritySetting{enabled: configured, expiresAt: time.Now().Add(-time.Second)})
		require.Equal(t, !configured, second.astraPriorityEnabled(ctx))
		restarted := NewSettingService(repo, cfg)
		require.Equal(t, !configured, restarted.astraPriorityEnabled(ctx), "restart must preserve the saved override")
		require.NoError(t, first.SetAstraPrioritySettings(ctx, configured))
		require.Equal(t, configured, first.astraPriorityEnabled(ctx))
	}
}

func TestAstraPriorityRuntimeRetainsSavedOffOnDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled = true
	repo := &panelRateLimitSettingRepo{}
	svc := NewSettingService(repo, cfg)
	require.NoError(t, svc.SetAstraPrioritySettings(ctx, false))
	repo.getValueErr = errors.New("database unavailable")
	svc.astraPriorityCache.Store(&cachedAstraPrioritySetting{enabled: false, expiresAt: time.Now().Add(-time.Second)})
	require.False(t, svc.astraPriorityEnabled(ctx))
	_, err := svc.GetAstraPrioritySettings(ctx)
	require.Error(t, err, "the admin must see the persistence read error")

	svc.settingRepo = &astraPriorityWriteFailureRepo{repo}
	require.Error(t, svc.SetAstraPrioritySettings(ctx, true))
	require.False(t, svc.astraPriorityEnabled(ctx), "failed save must leave the effective choice unchanged")
}

func TestAstraPriorityRuntimeToggleChangesSelectionWithoutRestart(t *testing.T) {
	ctx := context.Background()
	accounts := astraPriorityTestAccounts()
	svc := newAstraPriorityTestService(accounts, false, false, true, schedulerTestConcurrencyCache{})
	svc.settingService = NewSettingService(&panelRateLimitSettingRepo{}, svc.cfg)
	bindAstraPriorityTestSession(t, svc, accounts[1].ID)
	first, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
	require.Equal(t, accounts[0].ID, first.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPriorityFirst, decision.Layer)
	require.NoError(t, svc.settingService.SetAstraPrioritySettings(ctx, false))
	bindAstraPriorityTestSession(t, svc, accounts[1].ID)
	second, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
	require.Equal(t, accounts[1].ID, second.Account.ID)
	require.NotEqual(t, openAIAccountScheduleLayerPriorityFirst, decision.Layer)
	require.NoError(t, svc.settingService.SetAstraPrioritySettings(ctx, true))
	third, decision := selectAstraPriorityTestAccount(t, svc, "gpt-6", "", false, nil)
	require.Equal(t, accounts[0].ID, third.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPriorityFirst, decision.Layer)
	require.False(t, svc.preferOpenAIAstraPriority(ctx, PlatformOpenAI, "gpt-6-sol"))
	require.False(t, svc.preferOpenAIAstraPriority(ctx, PlatformGrok, "gpt-6-astra"))
}
