package admin

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) GetAccountSchedulingMetadata(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "排序及租期设置仅限完整管理员访问")
		return
	}
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效")
		return
	}
	if err := service.ValidateExtensionIDs(req.AccountIDs); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	accounts, err := h.adminService.GetAccountsByIDs(c.Request.Context(), req.AccountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items := make([]service.AccountSchedulingMetadata, 0, len(accounts))
	for _, a := range accounts {
		if a != nil {
			items = append(items, service.SchedulingMetadata(a))
		}
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, items)
}

func (h *AccountHandler) GetAccountSchedulingPreview(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "排序设置仅限完整管理员访问")
		return
	}
	groupID, err := strconv.ParseInt(c.DefaultQuery("group_id", "0"), 10, 64)
	if err != nil || groupID < 0 {
		response.BadRequest(c, "分组 ID 无效")
		return
	}
	platform := c.DefaultQuery("platform", service.PlatformOpenAI)
	svc, ok := h.adminService.(service.AccountSchedulingAdmin)
	if !ok {
		response.Error(c, 503, "排序服务不可用")
		return
	}
	accounts, err := svc.AccountSchedulingCandidates(c.Request.Context(), groupID, platform)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, service.BuildAccountSchedulingPreview(accounts, h.cfg, time.Now()))
}
