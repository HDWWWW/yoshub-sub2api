package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetOpenAIImportThreshold(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "导入默认设置仅限完整管理员访问")
		return
	}
	c.Header("Cache-Control", "no-store")
	threshold, err := h.settingService.GetOpenAIImportThreshold(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"percent": threshold})
}

func (h *SettingHandler) SetOpenAIImportThreshold(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "导入默认设置仅限完整管理员访问")
		return
	}
	var req struct {
		Percent int `json:"percent" binding:"required,min=1,max=100"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请输入 1–100 的整数百分比")
		return
	}
	middleware.SetAuditAction(c, "admin.accounts.import_threshold.update")
	if err := h.settingService.SetOpenAIImportThreshold(c.Request.Context(), req.Percent); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"percent": req.Percent})
}
