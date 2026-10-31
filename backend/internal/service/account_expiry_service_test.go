package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type expiryMaintenanceRepo struct {
	AccountRepository
	priorityCalls atomic.Int32
	pauseCalls    atomic.Int32
}

func (r *expiryMaintenanceRepo) SyncAccountExpiryPriorities(ctx context.Context, now time.Time) (int64, error) {
	r.priorityCalls.Add(1)
	return 0, nil
}

func (r *expiryMaintenanceRepo) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	r.pauseCalls.Add(1)
	return 0, nil
}

func TestAccountExpiryServiceRunsAtStartupAndRepeats(t *testing.T) {
	repo := &expiryMaintenanceRepo{}
	svc := NewAccountExpiryService(repo, 10*time.Millisecond)
	svc.expiryPriorityEnabled = true
	svc.Start()
	t.Cleanup(svc.Stop)
	require.Eventually(t, func() bool {
		return repo.pauseCalls.Load() >= 2
	}, time.Second, time.Millisecond)
	svc.Stop()
	priorities, pauses := repo.priorityCalls.Load(), repo.pauseCalls.Load()
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, priorities, repo.priorityCalls.Load())
	require.Equal(t, pauses, repo.pauseCalls.Load())
}

func TestAccountExpiryServiceNeverRewritesNativePriority(t *testing.T) {
	repo := &expiryMaintenanceRepo{}
	svc := NewAccountExpiryService(repo, time.Minute)
	svc.expiryPriorityEnabled = true
	svc.runOnce()
	require.Zero(t, repo.priorityCalls.Load())
	require.EqualValues(t, 1, repo.pauseCalls.Load())
}

func TestAccountExpiryServiceConfigGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[enabled], func(t *testing.T) {
			repo := &expiryMaintenanceRepo{}
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = enabled
			svc := ProvideAccountExpiryService(repo, cfg)
			t.Cleanup(svc.Stop)
			require.Eventually(t, func() bool { return repo.pauseCalls.Load() > 0 }, time.Second, time.Millisecond)
			svc.Stop()
			require.Equal(t, enabled, svc.expiryPriorityEnabled)
			require.Zero(t, repo.priorityCalls.Load())
		})
	}
}
