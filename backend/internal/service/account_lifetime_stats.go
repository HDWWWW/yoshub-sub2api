package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

func (s *AccountUsageService) GetLifetimeStatsBatch(ctx context.Context, ids []int64) (map[int64]*WindowStats, error) {
	if len(ids) > 1000 || (adminaccess.RestrictedFromContext(ctx) != nil && len(ids) > 100) {
		return nil, fmt.Errorf("账户数量超过限制")
	}
	unique := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		if adminaccess.RestrictedFromContext(ctx) != nil {
			if err := scopedAccountIDAccess(ctx, s.accountRepo, id, "accounts.stats.read", false); err != nil {
				return nil, err
			}
		}
		seen[id] = true
		unique = append(unique, id)
	}
	reader, ok := s.usageLogRepo.(interface {
		GetAccountLifetimeStatsBatch(context.Context, []int64) (map[int64]*usagestats.AccountStats, error)
	})
	if !ok {
		return nil, fmt.Errorf("账号累计统计不可用")
	}
	stats, err := reader.GetAccountLifetimeStatsBatch(ctx, unique)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*WindowStats, len(stats))
	for id, value := range stats {
		result[id] = windowStatsFromAccountStats(value)
	}
	return result, nil
}
