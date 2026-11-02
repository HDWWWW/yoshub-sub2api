package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetDefaultAccountRetryCount(c *gin.Context) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "默认重试设置仅限完整管理员访问")
		return
	}
	c.Header("Cache-Control", "no-store")
	settings, err := h.settingService.GetDefaultAccountRetryCount(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"count": settings})
}

func (h *SettingHandler) SetDefaultAccountRetryCount(c *gin.Context) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "默认重试设置仅限完整管理员访问")
		return
	}
	var request struct {
		Count *int `json:"count" binding:"required,gte=0,lte=10"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "必须提供 0–10 的整数次数")
		return
	}
	middleware.SetAuditAction(c, "admin.accounts.default_retry.update")
	if err := h.settingService.SetDefaultAccountRetryCount(c.Request.Context(), *request.Count); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"count": *request.Count})
}
