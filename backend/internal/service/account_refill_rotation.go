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

type refillRoute struct{ provider, plan string }

func refillNextPlan(c AccountRefillConfig, cursor int) string {
	return refillNextRoute(c, cursor).plan
}

func refillNextRoute(c AccountRefillConfig, cursor int) refillRoute {
	primary := c.automaticProvider()
	total := refillRotationLength(c)
	if !c.RotationEnabled || total <= 0 {
		return refillRoute{primary, c.Plan}
	}
	cursor = max(0, cursor) % total
	for _, step := range c.Rotation {
		if cursor < step.Count {
			switch step.Provider {
			case "1208":
				return refillRoute{"", step.Plan}
			case refillProviderMuze:
				return refillRoute{refillProviderMuze, step.Plan}
			default:
				return refillRoute{primary, step.Plan}
			}
		}
		cursor -= step.Count
	}
	return refillRoute{primary, c.Plan}
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
