package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

const muzeCatalogFreshness = 5 * time.Minute

type MuzeRemainingNotificationConfig struct {
	Enabled   bool     `json:"enabled"`
	Threshold int      `json:"threshold"`
	Products  []string `json:"products"`
}

func (c *MuzeRemainingNotificationConfig) enabled() bool { return c != nil && c.Enabled }

func (c *MuzeRemainingNotificationConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.Threshold < 0 || c.Threshold > 10_000_000 {
		return errors.New("Muze 次数提醒阈值须为 0–10000000 的整数")
	}
	if c.Enabled && len(c.Products) == 0 {
		return errors.New("请至少选择一个 Muze 提醒套餐")
	}
	if len(c.Products) > 64 {
		return errors.New("最多可选择 64 个 Muze 提醒套餐")
	}
	seen := make(map[string]bool, len(c.Products))
	for _, id := range c.Products {
		if !validMuzeIdentifier(id, false) || seen[id] {
			return errors.New("Muze 提醒套餐标识无效或重复")
		}
		seen[id] = true
	}
	return nil
}

// Versioned SKUs share the prepaid allowance of their tier. An unfamiliar
// catalog item can be monitored independently even before it supports ordering.
func muzeAllowanceKey(id string) string {
	if tier := muzeTier(id); tier != "" {
		return tier
	}
	return id
}

func (c *MuzeRemainingNotificationConfig) pruneAlerts(alerted map[string]bool) {
	if !c.enabled() {
		clear(alerted)
		return
	}
	keep := make(map[string]bool, len(c.Products))
	for _, id := range c.Products {
		keep[muzeAllowanceKey(id)] = true
	}
	for key := range alerted {
		if !keep[key] {
			delete(alerted, key)
		}
	}
}

// The background worker consumes confirmed catalog snapshots, including a fresh
// one fetched by the UI or a quote. Failed reads and missing SKUs are unknown.
func (s *AccountRefillService) notifyMuzeRemaining(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	c := state.Config.Notifications.MuzeRemaining
	f := &state.Muze
	if !c.enabled() || !refillNotificationConfigured(state) || f.EncryptedToken == "" || f.CheckedAt == nil || now.Sub(*f.CheckedAt) >= muzeCatalogFreshness || !f.RetryAt.IsZero() {
		return nil
	}
	samples := make(map[string]MuzeProduct)
	for _, p := range f.Products {
		if p.Remaining < 0 || !slices.Contains(c.Products, p.Product) {
			continue
		}
		key := muzeAllowanceKey(p.Product)
		if current, ok := samples[key]; !ok || p.Remaining < current.Remaining || (p.Remaining == current.Remaining && p.Product < current.Product) {
			samples[key] = p
		}
	}
	var changed bool
	for _, key := range slices.Sorted(maps.Keys(samples)) {
		p := samples[key]
		if p.Remaining > c.Threshold {
			if state.MuzeRemainingAlerted[key] {
				delete(state.MuzeRemainingAlerted, key)
				changed = true
			}
			continue
		}
		if state.MuzeRemainingAlerted[key] {
			continue
		}
		if state.MuzeRemainingAlerted == nil {
			state.MuzeRemainingAlerted = map[string]bool{}
		}
		state.MuzeRemainingAlerted[key] = true
		// Reserve the attempt under the rule lock before any external side effect.
		if err := persist(); err != nil {
			return err
		}
		content := fmt.Sprintf("Sub2API Muze 剩余次数不足提醒\n监测分组：#%d\n套餐：%s\n剩余次数：%d\n提醒阈值：%d\n北京时间：%s", state.Config.GroupID, p.Product, p.Remaining, c.Threshold, formatNotificationTime(*f.CheckedAt))
		s.sendRefillNotification(ctx, state, content)
		changed = true
	}
	if changed {
		return persist()
	}
	return nil
}
