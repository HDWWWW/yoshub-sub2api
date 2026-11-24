package service

import (
	"errors"
	"slices"
)

func validateRefillRotation(c AccountRefillConfig) error {
	if len(c.Rotation) > 20 || (c.RotationEnabled && len(c.Rotation) == 0) {
		return errors.New("轮转补号须设置 1–20 个步骤")
	}
	for _, step := range c.Rotation {
		if !slices.Contains([]string{"", "1208", refillProviderMuze}, step.Provider) {
			return errors.New("轮转步骤的号商须为 1208、Muze 或跟随分组主号商")
		}
		if step.MuzeProductID != "" {
			provider := step.Provider
			if provider == "" {
				provider = c.automaticProvider()
			}
			if provider != refillProviderMuze || !validMuzeIdentifier(step.MuzeProductID, false) || muzeTier(step.MuzeProductID) != step.Plan {
				return errors.New("指定的 Muze 商品须与轮转步骤的号商和套餐档位一致")
			}
		}
		if !slices.Contains([]string{"pro5x", "pro20x"}, step.Plan) || step.Count < 1 || step.Count > 100 {
			return errors.New("轮转步骤须选择有效套餐，数量须为 1–100")
		}
	}
	return nil
}

func refillRotationLength(c AccountRefillConfig) int {
	total := 0
	for _, step := range c.Rotation {
		total += step.Count
	}
	return total
}

type refillRoute struct {
	provider, plan string
	muzeProductID  string // Optional exact SKU. plan always holds the local tier.
}

func (j AccountRefillJob) purchaseRoute() refillRoute {
	route := refillRoute{provider: j.Provider, plan: j.Plan}
	if j.MuzeProductPinned && j.Provider == refillProviderMuze {
		route.muzeProductID = j.MuzeProductID
	}
	return route
}

func refillNextPlan(c AccountRefillConfig, cursor int) string {
	return refillNextRoute(c, cursor).plan
}

func refillNextRoute(c AccountRefillConfig, cursor int) refillRoute {
	primary := c.automaticProvider()
	total := refillRotationLength(c)
	if !c.RotationEnabled || total <= 0 {
		return refillRoute{provider: primary, plan: c.Plan}
	}
	cursor = max(0, cursor) % total
	for _, step := range c.Rotation {
		if cursor < step.Count {
			provider := primary
			switch step.Provider {
			case "1208":
				provider = ""
			case refillProviderMuze:
				provider = refillProviderMuze
			}
			return refillRoute{provider: provider, plan: step.Plan, muzeProductID: step.MuzeProductID}
		}
		cursor -= step.Count
	}
	return refillRoute{provider: primary, plan: c.Plan}
}

func (c AccountRefillConfig) needsAutomaticProvider(provider string) bool {
	followsRotation := c.Enabled
	tasks := c.DailySchedules
	if tasks == nil && c.DailySchedule != nil {
		tasks = []AccountRefillDailySchedule{*c.DailySchedule}
	}
	for _, task := range tasks {
		if task.Enabled {
			if task.Plan != "" && c.automaticProvider() == provider {
				return true
			}
			followsRotation = followsRotation || task.Plan == ""
		}
	}
	if !followsRotation {
		return false
	}
	if !c.RotationEnabled {
		return c.automaticProvider() == provider
	}
	cursor := 0
	for _, step := range c.Rotation {
		if refillNextRoute(c, cursor).provider == provider {
			return true
		}
		cursor += step.Count
	}
	return false
}

// Advance only with a new automatic demand reservation. Both uncertain retries
// and confirmed-failure replacements retain the original rotation slot.
func advanceRefillRotation(state *accountRefillState) {
	if total := refillRotationLength(state.Config); state.Config.RotationEnabled && total > 0 {
		state.RotationCursor = (max(0, state.RotationCursor) + 1) % total
	}
}
