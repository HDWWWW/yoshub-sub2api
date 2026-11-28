package admin

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ProxyHandler) ovhIPv6(c *gin.Context, update bool) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "OVH IPv6 设置仅限完整管理员访问")
		return
	}
	svc, ok := h.adminService.(service.OVHIPv6Admin)
	if !ok {
		response.Error(c, 503, "OVH IPv6 服务不可用")
		return
	}
	c.Header("Cache-Control", "no-store")
	pool, valid := ovhipv6.ParsePool(c.Query("pool"))
	if !valid {
		response.BadRequest(c, "IPv6 地址池无效")
		return
	}
	ctx := ovhipv6.WithPool(c.Request.Context(), pool)
	if update {
		var request struct {
			Enabled *bool `json:"enabled" binding:"required"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			response.BadRequest(c, "必须提供 enabled 布尔值")
			return
		}
		middleware.SetAuditAction(c, "admin.proxies.ovh_ipv6.update")
		status, err := svc.SetOVHIPv6(ctx, *request.Enabled)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		response.Success(c, status)
		return
	}
	status, err := svc.GetOVHIPv6(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *ProxyHandler) GetOVHIPv6(c *gin.Context) { h.ovhIPv6(c, false) }
func (h *ProxyHandler) SetOVHIPv6(c *gin.Context) { h.ovhIPv6(c, true) }

func (h *ProxyHandler) ListIPv6Retirements(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "IPv6 退役记录仅限完整管理员访问")
		return
	}
	before, err := strconv.ParseInt(c.DefaultQuery("before", "0"), 10, 64)
	if err != nil || before < 0 {
		response.BadRequest(c, "无效的分页位置")
		return
	}
	svc, ok := h.adminService.(interface {
		ListIPv6Retirements(context.Context, int64) ([]ovhipv6.Retirement, error)
	})
	if !ok {
		response.Error(c, 503, "IPv6 退役记录暂不可用")
		return
	}
	items, err := svc.ListIPv6Retirements(c.Request.Context(), before)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *ProxyHandler) AllocateOVHIPv6(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "OVH IPv6 预分配仅限完整管理员访问")
		return
	}
	var req struct {
		Name string `json:"name" binding:"max=100"`
		Pool string `json:"pool,omitempty" binding:"omitempty,oneof=primary backup"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求须为 JSON 对象，name 最多 100 字符")
		return
	}
	svc, ok := h.adminService.(interface {
		AllocateOVHIPv6(context.Context, string) (*service.Proxy, error)
	})
	if !ok {
		response.Error(c, 503, "OVH IPv6 服务不可用")
		return
	}
	c.Header("Cache-Control", "no-store")
	middleware.SetAuditAction(c, "admin.proxies.ovh_ipv6.allocate")
	executeAdminIdempotentJSON(c, "admin.proxies.ovh_ipv6.allocate", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		pool, _ := ovhipv6.ParsePool(req.Pool)
		p, err := svc.AllocateOVHIPv6(ovhipv6.WithPool(ctx, pool), req.Name)
		if err != nil {
			return nil, err
		}
		return dto.ProxyFromService(p), nil
	})
}
