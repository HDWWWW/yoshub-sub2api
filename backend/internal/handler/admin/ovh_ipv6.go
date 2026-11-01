package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ProxyHandler) ovhIPv6(c *gin.Context, update bool) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "OVH IPv6 设置仅限完整管理员访问")
		return
	}
	svc, ok := h.adminService.(service.OVHIPv6Admin)
	if !ok {
		response.Error(c, 503, "OVH IPv6 服务不可用")
		return
	}
	c.Header("Cache-Control", "no-store")
	if update {
		var request struct {
			Enabled *bool `json:"enabled" binding:"required"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "必须提供 enabled 布尔值")
			return
		}
		middleware.SetAuditAction(c, "admin.proxies.ovh_ipv6.update")
		status, err := svc.SetOVHIPv6(c.Request.Context(), *request.Enabled)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		response.Success(c, status)
		return
	}
	status, err := svc.GetOVHIPv6(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *ProxyHandler) GetOVHIPv6(c *gin.Context) { h.ovhIPv6(c, false) }
func (h *ProxyHandler) SetOVHIPv6(c *gin.Context) { h.ovhIPv6(c, true) }
