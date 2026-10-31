package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SettingKeyHappyFirstToken = "happy_first_token_settings"

// HappyFirstTokenSettings controls a quota-free SSE preamble, not model TTFT.
type HappyFirstTokenGroupOverride struct {
	GroupID    int64 `json:"group_id"`
	Enabled    bool  `json:"enabled"`
	MinDelayMS int   `json:"min_delay_ms"`
	MaxDelayMS int   `json:"max_delay_ms"`
}

type HappyFirstTokenSettings struct {
	GroupOverrides []HappyFirstTokenGroupOverride `json:"group_overrides,omitempty"`
	Enabled        bool                           `json:"enabled"`
	MinDelayMS     int                            `json:"min_delay_ms"`
	MaxDelayMS     int                            `json:"max_delay_ms"`
}

type cachedHappyFirstTokenSettings struct {
	settings  HappyFirstTokenSettings
	expiresAt time.Time
}

func DefaultHappyFirstTokenSettings() HappyFirstTokenSettings {
	return HappyFirstTokenSettings{MinDelayMS: 100, MaxDelayMS: 1000}
}

func (v HappyFirstTokenSettings) Validate() error {
	if v.MinDelayMS < 0 || v.MaxDelayMS > 60000 || v.MinDelayMS > v.MaxDelayMS {
		return fmt.Errorf("delay range must satisfy 0 <= min_delay_ms <= max_delay_ms <= 60000")
	}
	if len(v.GroupOverrides) > 1000 {
		return errors.New("too many group overrides")
	}
	seen := map[int64]bool{}
	for _, g := range v.GroupOverrides {
		if g.GroupID <= 0 || seen[g.GroupID] {
			return errors.New("group overrides require distinct positive group IDs")
		}
		seen[g.GroupID] = true
		if g.MinDelayMS < 0 || g.MaxDelayMS > 60000 || g.MinDelayMS > g.MaxDelayMS {
			return errors.New("invalid group delay range")
		}
	}
	return nil
}

// ForGroup uses the request API key group, never all groups of a shared account.
func (v HappyFirstTokenSettings) ForGroup(id int64) HappyFirstTokenSettings {
	for _, g := range v.GroupOverrides {
		if g.GroupID == id {
			return HappyFirstTokenSettings{Enabled: g.Enabled, MinDelayMS: g.MinDelayMS, MaxDelayMS: g.MaxDelayMS}
		}
	}
	return HappyFirstTokenSettings{Enabled: v.Enabled, MinDelayMS: v.MinDelayMS, MaxDelayMS: v.MaxDelayMS}
}

func (s *SettingService) GetHappyFirstTokenSettings(ctx context.Context) (HappyFirstTokenSettings, error) {
	v := DefaultHappyFirstTokenSettings()
	if s == nil || s.settingRepo == nil {
		return v, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyHappyFirstToken)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && strings.TrimSpace(raw) == "") {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return DefaultHappyFirstTokenSettings(), fmt.Errorf("decode happy first token settings: %w", err)
	}
	if err = v.Validate(); err != nil {
		return DefaultHappyFirstTokenSettings(), err
	}
	return v, nil
}

func (s *SettingService) UpdateHappyFirstTokenSettings(ctx context.Context, v HappyFirstTokenSettings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	for _, g := range v.GroupOverrides {
		if s.defaultSubGroupReader == nil {
			return errors.New("group validation is unavailable")
		}
		group, err := s.defaultSubGroupReader.GetByID(ctx, g.GroupID)
		if err != nil || group == nil || group.Platform != PlatformOpenAI {
			return errors.New("请选择有效的 OpenAI 分组")
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.happyFirstTokenMu.Lock()
	defer s.happyFirstTokenMu.Unlock()
	if err = s.settingRepo.Set(ctx, SettingKeyHappyFirstToken, string(data)); err != nil {
		return err
	}
	s.happyFirstTokenCache.Store(&cachedHappyFirstTokenSettings{settings: v, expiresAt: time.Now().Add(30 * time.Second)})
	return nil
}

func (s *SettingService) happyFirstTokenSettings(ctx context.Context) HappyFirstTokenSettings {
	if s == nil || s.settingRepo == nil {
		return DefaultHappyFirstTokenSettings()
	}
	if cached := s.happyFirstTokenCache.Load(); cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.settings
	}
	s.happyFirstTokenMu.Lock()
	defer s.happyFirstTokenMu.Unlock()
	if cached := s.happyFirstTokenCache.Load(); cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.settings
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := s.GetHappyFirstTokenSettings(dbCtx)
	ttl := 30 * time.Second
	if err != nil {
		v, ttl = DefaultHappyFirstTokenSettings(), 5*time.Second
	}
	s.happyFirstTokenCache.Store(&cachedHappyFirstTokenSettings{settings: v, expiresAt: time.Now().Add(ttl)})
	return v
}
