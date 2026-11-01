package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetAstraPriority(c *gin.Context) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "调度设置仅限完整管理员访问")
		return
	}
	c.Header("Cache-Control", "no-store")
	settings, err := h.settingService.GetAstraPrioritySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) SetAstraPriority(c *gin.Context) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "调度设置仅限完整管理员访问")
		return
	}
	var request struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "必须提供 enabled 布尔值")
		return
	}
	middleware.SetAuditAction(c, "admin.accounts.astra_priority.update")
	if err := h.settingService.SetAstraPrioritySettings(c.Request.Context(), *request.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, service.AstraPrioritySettings{Enabled: *request.Enabled})
}
