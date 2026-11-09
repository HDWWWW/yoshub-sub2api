package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type GroupFirstTokenSettings struct {
	EarlyRateLimitsEnabled bool `json:"early_rate_limits_enabled"`
}

type cachedGroupFirstToken struct {
	value   GroupFirstTokenSettings
	expires time.Time
}

func groupFirstTokenKey(id int64) string { return fmt.Sprintf("group_first_token_optimization:%d", id) }

func (s *SettingService) GetGroupFirstTokenSettings(ctx context.Context, id int64) (GroupFirstTokenSettings, error) {
	var v GroupFirstTokenSettings
	if s == nil || s.settingRepo == nil || id <= 0 {
		return v, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, groupFirstTokenKey(id))
	if errors.Is(err, ErrSettingNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if raw != "" {
		err = json.Unmarshal([]byte(raw), &v)
	}
	return v, err
}

func (s *SettingService) UpdateGroupFirstTokenSettings(ctx context.Context, id int64, v GroupFirstTokenSettings) error {
	if s == nil || s.settingRepo == nil || s.defaultSubGroupReader == nil || id <= 0 {
		return errors.New("group settings unavailable")
	}
	g, err := s.defaultSubGroupReader.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if g == nil || (g.Platform != PlatformOpenAI && g.Platform != PlatformComposite) {
		return errors.New("请选择有效的 OpenAI 或混合分组")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = s.settingRepo.Set(ctx, groupFirstTokenKey(id), string(data)); err != nil {
		return err
	}
	s.groupFirstTokenCache.Store(id, cachedGroupFirstToken{v, time.Now().Add(5 * time.Second)})
	return nil
}

func (s *SettingService) groupEarlyRateLimitsEnabled(ctx context.Context, id int64) bool {
	if s == nil || s.settingRepo == nil || id <= 0 {
		return false
	}
	if cached, ok := s.groupFirstTokenCache.Load(id); ok {
		v, valid := cached.(cachedGroupFirstToken)
		if valid && time.Now().Before(v.expires) {
			return v.value.EarlyRateLimitsEnabled
		}
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := s.GetGroupFirstTokenSettings(dbCtx, id)
	if err != nil {
		return false
	}
	// Bound the process cache across group churn; no credentials or request data.
	s.groupFirstTokenCache.Range(func(k, value any) bool {
		v, valid := value.(cachedGroupFirstToken)
		if !valid || time.Now().After(v.expires) {
			s.groupFirstTokenCache.Delete(k)
		}
		return true
	})
	s.groupFirstTokenCache.Store(id, cachedGroupFirstToken{v, time.Now().Add(5 * time.Second)})
	return v.EarlyRateLimitsEnabled
}
