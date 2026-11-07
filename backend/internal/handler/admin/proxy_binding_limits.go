package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ProxyHandler) proxyBindingLimits(c *gin.Context, update bool) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "绑定上限设置仅限完整管理员访问")
		return
	}
	var req struct {
		ProxyIDs    []int64 `json:"proxy_ids"`
		MaxAccounts int     `json:"max_accounts"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效")
		return
	}
	if err := service.ValidateExtensionIDs(req.ProxyIDs); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	svc, ok := h.adminService.(service.ProxyBindingLimitsAdmin)
	if !ok {
		response.Error(c, 503, "绑定上限服务不可用")
		return
	}
	if update {
		middleware.SetAuditAction(c, "admin.proxies.binding_limits.update")
		if err := svc.SetProxyBindingLimits(c.Request.Context(), req.ProxyIDs, req.MaxAccounts); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	items, err := svc.GetProxyBindingLimits(c.Request.Context(), req.ProxyIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, items)
}
func (h *ProxyHandler) GetProxyBindingLimits(c *gin.Context) { h.proxyBindingLimits(c, false) }
func (h *ProxyHandler) SetProxyBindingLimits(c *gin.Context) { h.proxyBindingLimits(c, true) }
