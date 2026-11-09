package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetGroupFirstTokenSettings(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid group id")
		return
	}
	v, err := h.settingService.GetGroupFirstTokenSettings(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, v)
}

func (h *SettingHandler) UpdateGroupFirstTokenSettings(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid group id")
		return
	}
	var req struct {
		Enabled *bool `json:"early_rate_limits_enabled" binding:"required"`
	}
	if err = c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "early_rate_limits_enabled is required")
		return
	}
	v := service.GroupFirstTokenSettings{EarlyRateLimitsEnabled: *req.Enabled}
	if err = h.settingService.UpdateGroupFirstTokenSettings(c.Request.Context(), id, v); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, v)
}
