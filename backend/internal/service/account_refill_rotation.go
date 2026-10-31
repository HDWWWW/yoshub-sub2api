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

func refillNextPlan(c AccountRefillConfig, cursor int) string {
	total := refillRotationLength(c)
	if !c.RotationEnabled || total <= 0 {
		return c.Plan
	}
	cursor = max(0, cursor) % total
	for _, step := range c.Rotation {
		if cursor < step.Count {
			return step.Plan
		}
		cursor -= step.Count
	}
	return c.Plan
}

// Advance only with the durable paid-job reservation. Retries reuse that job's
// plan and never consume another slot; manual orders do not change the cursor.
func advanceRefillRotation(state *accountRefillState) {
	if total := refillRotationLength(state.Config); state.Config.RotationEnabled && total > 0 {
		state.RotationCursor = (max(0, state.RotationCursor) + 1) % total
	}
}
