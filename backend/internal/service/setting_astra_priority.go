package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const SettingKeyOpenAIAstraPriorityEnabled = "openai_astra_priority_enabled"
const astraPriorityCacheTTL = 5 * time.Second

type AstraPrioritySettings struct {
	Enabled bool `json:"enabled"`
}

type cachedAstraPrioritySetting struct {
	enabled   bool
	expiresAt time.Time
}

func (s *SettingService) defaultAstraPriorityEnabled() bool {
	return s == nil || s.cfg == nil || s.cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled
}

func (s *SettingService) readAstraPriorityEnabled(ctx context.Context) (bool, error) {
	enabled := s.defaultAstraPriorityEnabled()
	if s == nil || s.settingRepo == nil {
		return enabled, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIAstraPriorityEnabled)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && strings.TrimSpace(raw) == "") {
		return enabled, nil
	}
	if err != nil {
		return enabled, err
	}
	if raw != "true" && raw != "false" {
		return enabled, fmt.Errorf("invalid Astra priority setting")
	}
	return raw == "true", nil
}

// Admin reads bypass the cache, so the account page shows the persisted choice.
func (s *SettingService) GetAstraPrioritySettings(ctx context.Context) (AstraPrioritySettings, error) {
	if s == nil {
		return AstraPrioritySettings{Enabled: true}, nil
	}
	s.astraPriorityMu.Lock()
	defer s.astraPriorityMu.Unlock()
	enabled, err := s.readAstraPriorityEnabled(ctx)
	if err == nil {
		s.astraPriorityCache.Store(&cachedAstraPrioritySetting{enabled: enabled, expiresAt: time.Now().Add(astraPriorityCacheTTL)})
	}
	return AstraPrioritySettings{Enabled: enabled}, err
}

func (s *SettingService) SetAstraPrioritySettings(ctx context.Context, enabled bool) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("Astra priority settings are unavailable")
	}
	s.astraPriorityMu.Lock()
	defer s.astraPriorityMu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAIAstraPriorityEnabled, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	s.astraPriorityCache.Store(&cachedAstraPrioritySetting{enabled: enabled, expiresAt: time.Now().Add(astraPriorityCacheTTL)})
	return nil
}

// Scheduling reads a per-service cache. Expired entries refresh without blocking
// concurrent selections. On DB failure retain the last successful choice;
// disabling priority must not silently revert to the environment's default.
func (s *SettingService) astraPriorityEnabled(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return s.defaultAstraPriorityEnabled()
	}
	cached := s.astraPriorityCache.Load()
	if cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.enabled
	}
	if cached != nil && !s.astraPriorityMu.TryLock() {
		return cached.enabled
	}
	if cached == nil {
		s.astraPriorityMu.Lock()
	}
	defer s.astraPriorityMu.Unlock()
	cached = s.astraPriorityCache.Load()
	if cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.enabled
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	enabled, err := s.readAstraPriorityEnabled(dbCtx)
	if err != nil && cached != nil {
		enabled = cached.enabled
	}
	s.astraPriorityCache.Store(&cachedAstraPrioritySetting{enabled: enabled, expiresAt: time.Now().Add(astraPriorityCacheTTL)})
	return enabled
}
