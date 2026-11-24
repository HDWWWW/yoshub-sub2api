//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillViewResolvesHistoricalSupplierIDsWithoutWriting(t *testing.T) {
	s, store, provider, _ := refillHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		state.Jobs = []AccountRefillJob{
			{ID: "internal-original", SupplierID: "supplier-original", Status: "failed", ReplacementJobID: "internal-replacement",
				Error: "号商确认补号失败，预算预占已释放；已创建同套餐替补任务 internal-replacement"},
			{ID: "internal-replacement", SupplierID: "supplier-replacement", Status: "failed", ReplacesJobID: "internal-original", ReplacementJobID: "internal-success",
				ReasonDetail: "号商已确认原任务 internal-original 失败，沿用 pro20x 套餐自动重补。本次需求：保留历史备注",
				Error:        "号商确认补号失败，预算预占已释放；已创建同套餐替补任务 internal-success"},
			{ID: "internal-success", SupplierID: "supplier-success", Status: "imported", AccountID: 122, ReplacesJobID: "internal-replacement",
				ReasonDetail: "号商已确认原任务 internal-replacement 失败，沿用 pro20x 套餐自动重补"},
		}
	})
	before := store.raw
	view, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, before, store.raw, "reading history must not rewrite jobs or acquire the purchase lock")
	require.Empty(t, provider.submitted)
	require.Contains(t, view.Jobs[0].Error, "supplier-replacement")
	require.Contains(t, view.Jobs[1].Error, "supplier-success")
	require.Contains(t, view.Jobs[1].ReasonDetail, "supplier-original")
	require.Contains(t, view.Jobs[1].ReasonDetail, "保留历史备注")
	require.Contains(t, view.Jobs[2].ReasonDetail, "supplier-replacement")
	require.Equal(t, int64(122), view.Jobs[2].AccountID)
	for _, job := range view.Jobs {
		require.NotContains(t, job.Error+job.ReasonDetail, "internal-")
	}
	require.Equal(t, "internal-replacement", view.Jobs[1].ID)
	require.Equal(t, "internal-original", view.Jobs[1].ReplacesJobID)

	store.mutate(t, func(state *accountRefillState) { state.Jobs[2].SupplierID = "" })
	view, err = s.View(context.Background())
	require.NoError(t, err)
	require.Contains(t, view.Jobs[1].Error, "号商暂未返回 ID")
	store.mutate(t, func(state *accountRefillState) { state.Jobs[2].SupplierID = "supplier-returned-later" })
	view, err = s.View(context.Background())
	require.NoError(t, err)
	require.Contains(t, view.Jobs[1].Error, "supplier-returned-later")
}

func TestRefillDisplayHandlesPrunedReferencesAndNewReplacementNotes(t *testing.T) {
	state := accountRefillState{Jobs: []AccountRefillJob{{ID: "internal-original", SupplierID: "supplier-original", Plan: "pro20x"}}}
	job := AccountRefillJob{ID: "internal-replacement", ReasonDetail: "历史需求"}
	linkRefillFailureReplacement(&state, 0, &job)
	require.Contains(t, job.ReasonDetail, "原任务 supplier-original 失败")
	require.NotContains(t, state.Jobs[0].Error, job.ID)
	require.Equal(t, "internal-original", job.ReplacesJobID)
	require.Equal(t, job.ID, state.Jobs[0].ReplacementJobID)
	// New notes retain the supplier ID even after the original job is trimmed.
	require.Equal(t, job.ReasonDetail, refillJobsForDisplay([]AccountRefillJob{job})[0].ReasonDetail)
	legacy := AccountRefillJob{ReplacesJobID: "internal-pruned", ReplacementJobID: "internal-missing",
		ReasonDetail: "号商已确认原任务 internal-pruned 失败，历史需求",
		Error:        refillReplacementCreatedMessage + " internal-missing"}
	display := refillJobsForDisplay([]AccountRefillJob{legacy})[0]
	require.NotContains(t, display.ReasonDetail+display.Error, "internal-")
	require.Contains(t, display.ReasonDetail, "历史记录已清理")
	require.Contains(t, display.Error, "历史记录已清理")
}

func TestRefillNotificationsUseOnlySupplierTaskIDs(t *testing.T) {
	for _, stage := range []string{"started", "failed", "imported", "delayed", "delayed_without_supplier"} {
		t.Run(stage, func(t *testing.T) {
			s, _, _, _ := refillHarness(t)
			notifier := &refillNoticeStub{}
			s.notifier = notifier
			state := accountRefillState{Config: refillTestConfig(), EncryptedWebhook: "encrypted-fixture"}
			state.Config.Notifications.RefillStarted = true
			state.Jobs = []AccountRefillJob{{ID: "internal-not-for-display", SupplierID: "supplier-visible", Plan: "pro20x",
				Status: "queued", CreatedAt: time.Now().Add(-6 * time.Minute)}}
			job := &state.Jobs[0]
			persist := func() error { return nil }
			ctx := context.Background()
			switch stage {
			case "started":
				job.SupplierID = ""
				require.NoError(t, s.notifyRefillStarted(ctx, &state, job, persist))
			case "failed":
				job.Status, job.SupplierStatus = "failed", "failed"
				require.NoError(t, s.notifyRefillFailed(ctx, &state, job, persist))
			case "imported":
				job.Status, job.AccountID = "imported", 122
				require.NoError(t, s.notifyRefillImported(ctx, &state, job, persist))
			default:
				if stage == "delayed_without_supplier" {
					job.SupplierID = ""
				}
				require.NoError(t, s.notifyRefillDelayed(ctx, &state, time.Now(), persist))
			}
			require.Len(t, notifier.messages, 1)
			require.NotContains(t, notifier.messages[0], job.ID)
			if job.SupplierID == "" {
				require.Contains(t, notifier.messages[0], "号商暂未返回 ID")
			} else {
				require.Contains(t, notifier.messages[0], "号商任务：supplier-visible")
			}
		})
	}
}

func TestRefillHistorical1208SupplierExpiryOnly(t *testing.T) {
	stamp := int64(1795536900)
	jobs := []AccountRefillJob{{ExpiresAt: &stamp}, {Provider: "muze", ExpiresAt: &stamp}}
	display := refillJobsForDisplay(jobs)
	require.Equal(t, &stamp, display[0].SupplierExpiresAt)
	require.Nil(t, display[1].SupplierExpiresAt)
	require.Nil(t, jobs[0].SupplierExpiresAt)
}
