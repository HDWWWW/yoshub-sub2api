package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const AccountManualPriorityExtraKey = "yoshub_manual_scheduling_priority"

type manualPriorityWriteKey struct{}

// Used by persistence while merging a row locked against concurrent imports.
func HasManualSchedulingPriorityWrite(ctx context.Context) bool {
	value, _ := ctx.Value(manualPriorityWriteKey{}).(bool)
	return value
}

// An explicit extension write owns this value; ordinary supplier priority
// writes must neither enable nor erase the administrator's override.
func (a *Account) ManualSchedulingPriority() (int, bool) {
	if a == nil {
		return 0, false
	}
	n, err := strconv.Atoi(fmt.Sprint(a.Extra[AccountManualPriorityExtraKey]))
	return n, err == nil && n >= 0 && n <= 9999
}

func validateSchedulingPriorityMode(ctx context.Context, mode string, priority *int) error {
	if mode == "" {
		return nil
	}
	if adminaccess.FromContext(ctx) != nil {
		return infraerrors.Forbidden("SCHEDULING_PRIORITY_FORBIDDEN", "手动排序仅限完整管理员设置")
	}
	if mode != "manual" && mode != "auto" {
		return infraerrors.BadRequest("INVALID_SCHEDULING_PRIORITY_MODE", "排序模式须为 manual 或 auto")
	}
	if mode == "manual" && (priority == nil || *priority < 0 || *priority > 9999) {
		return infraerrors.BadRequest("INVALID_SCHEDULING_PRIORITY", "手动优先级须为 0–9999 的整数")
	}
	return nil
}

func applyManualSchedulingPriority(extra map[string]any, mode string, priority *int) map[string]any {
	if mode == "" {
		return extra
	}
	out := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		out[k] = v
	}
	out[AccountManualPriorityExtraKey] = nil // JSON null also resets via bulk merge.
	if mode == "manual" {
		out[AccountManualPriorityExtraKey] = *priority
	}
	return out
}

type AccountSchedulingMetadata struct {
	AccountID          int64      `json:"account_id"`
	SourceExpiresAt    *time.Time `json:"source_expires_at"`
	SourceExpiryOrigin string     `json:"source_expiry_origin"`
	EffectiveExpiresAt *time.Time `json:"effective_expires_at"`
	GraceMinutes       int        `json:"grace_minutes"`
	ManualPriority     *int       `json:"manual_priority"`
}

func SchedulingMetadata(a *Account) AccountSchedulingMetadata {
	source, origin := a.SourceExpiresAt()
	if origin == "local" {
		source = nil
	} // Never label a local date as supplier data.
	item := AccountSchedulingMetadata{
		AccountID: a.ID, SourceExpiresAt: source, SourceExpiryOrigin: origin,
		EffectiveExpiresAt: a.SchedulingExpiresAt(), GraceMinutes: a.ExpiryGraceSeconds() / 60,
	}
	if priority, ok := a.ManualSchedulingPriority(); ok {
		item.ManualPriority = &priority
	}
	return item
}

type AccountSchedulingPreviewItem struct {
	AccountSchedulingMetadata
	Name              string `json:"name"`
	NativePriority    int    `json:"native_priority"`
	AutomaticPriority int    `json:"automatic_priority"`
	EffectivePriority int    `json:"effective_priority"`
}
type AccountSchedulingPreview struct {
	ExpiryPriorityEnabled bool                           `json:"expiry_priority_enabled"`
	Items                 []AccountSchedulingPreviewItem `json:"items"`
}

// Preview the same candidate-local ranks the gateway uses. Runtime gates and
// sticky sessions may still change which account handles an actual request.
func BuildAccountSchedulingPreview(accounts []Account, cfg *config.Config, now time.Time) AccountSchedulingPreview {
	ranked := accountsWithExpiryPriority(accounts, cfg, now)
	result := AccountSchedulingPreview{
		ExpiryPriorityEnabled: cfg != nil && cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled,
		Items:                 make([]AccountSchedulingPreviewItem, 0, len(ranked)),
	}
	for i := range ranked {
		a := &ranked[i]
		automatic := a.Priority
		if result.ExpiryPriorityEnabled {
			automatic = a.schedulingPriority
		}
		result.Items = append(result.Items, AccountSchedulingPreviewItem{
			AccountSchedulingMetadata: SchedulingMetadata(a), Name: a.Name,
			NativePriority: a.Priority, AutomaticPriority: automatic, EffectivePriority: a.SchedulingPriority(),
		})
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if a.EffectivePriority != b.EffectivePriority {
			return a.EffectivePriority < b.EffectivePriority
		}
		return a.AccountID < b.AccountID
	})
	return result
}

type AccountSchedulingAdmin interface {
	AccountSchedulingCandidates(context.Context, int64, string) ([]Account, error)
}

func (s *adminServiceImpl) AccountSchedulingCandidates(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	if groupID > 0 {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
	}
	return s.accountRepo.ListSchedulableByPlatform(ctx, platform)
}
