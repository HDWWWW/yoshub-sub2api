package service

import (
	"context"
	"errors"
	"maps"
	"math"
	"time"
)

// Auto Fast is independent of purchasing and monitoring. Nil preserves the
// disabled default for existing rules; nil LeadMinutes follows the refill lead.
type AccountRefillAutoFastConfig struct {
	Enabled               bool    `json:"enabled"`
	LeadMinutes           *int    `json:"lead_minutes,omitempty"`
	StartRemainingPercent float64 `json:"start_remaining_percent"`
	StopMode              string  `json:"stop_mode"`
	StopUsed5hPercent     float64 `json:"stop_used_5h_percent"`
	StopUsed7dPercent     float64 `json:"stop_used_7d_percent"`
}

func validateRefillAutoFast(c *AccountRefillAutoFastConfig) error {
	if c == nil {
		return nil
	}
	if c.StopMode != "follow" && c.StopMode != "custom" {
		return errors.New("自动 Fast 关闭条件须选择跟随补号规则或自定义")
	}
	if c.LeadMinutes != nil && (*c.LeadMinutes < 0 || *c.LeadMinutes > 1440) {
		return errors.New("自动 Fast 提前时间须为 0–1440 分钟")
	}
	if math.IsNaN(c.StartRemainingPercent) || math.IsInf(c.StartRemainingPercent, 0) || c.StartRemainingPercent < 0 || c.StartRemainingPercent > 100 {
		return errors.New("自动 Fast 开启剩余额度须为 0–100%")
	}
	if c.StopMode == "custom" {
		for _, value := range []float64{c.StopUsed5hPercent, c.StopUsed7dPercent} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 100 {
				return errors.New("自动 Fast 自定义已用额度上限须大于 0 且不超过 100%")
			}
		}
	}
	return nil
}

func refillAutoFastEnabled(c AccountRefillConfig) bool {
	return c.AutoFast != nil && c.AutoFast.Enabled
}

func refillAccountAutoFast(c AccountRefillConfig, row AccountRefillAccount, activeUntil time.Time, now time.Time) bool {
	if !refillAutoFastEnabled(c) || row.SourceExpiresAt == nil || row.ExpirySource == "local" || row.ExpirySource == "unknown" ||
		row.State == "unavailable" || row.State == "expired" || !row.SourceExpiresAt.After(now) {
		return false
	}
	lead := c.LeadMinutes
	if c.AutoFast.LeadMinutes != nil {
		lead = *c.AutoFast.LeadMinutes
	}
	if lead <= 0 || row.SourceExpiresAt.After(now.Add(time.Duration(lead)*time.Minute)) {
		return false
	}
	active := activeUntil.Equal(*row.SourceExpiresAt)
	checked := 0
	for _, window := range []struct {
		remaining        *float64
		floor, usedLimit float64
	}{
		{row.Remaining5h, row.MinRemaining5h, c.AutoFast.StopUsed5hPercent},
		{row.Remaining7d, row.MinRemaining7d, c.AutoFast.StopUsed7dPercent},
	} {
		floor := window.floor
		if c.AutoFast.StopMode == "custom" {
			floor = 100 - window.usedLimit
		} else if floor <= 0 {
			continue // A zero refill floor disables that quota window.
		}
		checked++
		if window.remaining == nil || *window.remaining <= floor ||
			(!active && *window.remaining <= c.AutoFast.StartRemainingPercent) {
			return false
		}
	}
	return checked > 0
}

func markRefillAutoFastAccounts(state *accountRefillState, now time.Time) map[int64]time.Time {
	active := make(map[int64]time.Time)
	for i := range state.Evaluation.Accounts {
		row := &state.Evaluation.Accounts[i]
		row.AutoFast = refillAccountAutoFast(state.Config, *row, state.AutoFastActive[row.ID], now)
		if row.AutoFast {
			active[row.ID] = *row.SourceExpiresAt
		}
	}
	return active
}

type refillAutoFastSnapshot struct {
	revision  int64
	checkedAt time.Time
	accounts  map[int64]time.Time
}

// Published maps are immutable. CompareAndSwap prevents an older sampler from
// restoring a switch after Configure has saved a newer revision.
func (s *AccountRefillService) publishRefillAutoFast(groupID, revision int64, accounts map[int64]time.Time, now time.Time) {
	root := s.rootService()
	next := &refillAutoFastSnapshot{revision: revision, checkedAt: now, accounts: accounts}
	for {
		old, loaded := root.autoFast.LoadOrStore(groupID, next)
		if !loaded || old.(*refillAutoFastSnapshot).revision > revision {
			return
		}
		if root.autoFast.CompareAndSwap(groupID, old, next) {
			return
		}
	}
}

func (s *AccountRefillService) syncRefillAutoFast(ctx context.Context, state *accountRefillState, now time.Time) {
	active := markRefillAutoFastAccounts(state, now)
	if !maps.Equal(active, state.AutoFastActive) {
		// Persist only transitions. This keeps start/stop hysteresis across
		// restarts and blue/green instances, using the existing shared lock.
		err := s.store.WithLock(ctx, func(raw string, save func(string) error) error {
			current, err := decodeAccountRefillState(raw)
			if err != nil {
				return err
			}
			if current.Revision != state.Revision {
				return errors.New("自动 Fast 规则已更新")
			}
			current.AutoFastActive = active
			return saveRefillState(&current, save)
		})
		if err != nil {
			// Stop immediately on uncertainty; never rely on an expired sample.
			active = nil
		}
	}
	s.publishRefillAutoFast(state.Config.GroupID, state.Revision, active, now)
}

func (s *AccountRefillService) accountAutoFast(groupID, accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	value, ok := s.rootService().autoFast.Load(groupID)
	if !ok {
		return false
	}
	snapshot := value.(*refillAutoFastSnapshot)
	return now.Sub(snapshot.checkedAt) <= 3*time.Second && snapshot.accounts[accountID].After(now)
}
