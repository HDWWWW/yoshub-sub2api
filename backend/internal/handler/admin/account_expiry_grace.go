package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Keep extension settings on their own endpoints; native account DTOs stay intact.
func (h *AccountHandler) accountExpiryGrace(c *gin.Context, update bool) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "保留时间设置仅限完整管理员访问")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "账号 ID 无效")
		return
	}
	a, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if !a.IsOpenAIOAuth() || a.IsShadow() {
		response.BadRequest(c, "仅支持 OpenAI OAuth 账号")
		return
	}
	if update {
		middleware.SetAuditAction(c, "admin.accounts.expiry_grace.update")
		var req struct {
			Minutes *int `json:"minutes"`
		}
		if c.ShouldBindJSON(&req) != nil || req.Minutes == nil || *req.Minutes < 0 || *req.Minutes > 1440 {
			response.BadRequest(c, "保留时间须为 0–1440 的整数分钟")
			return
		}
		a, err = h.adminService.UpdateAccount(c.Request.Context(), id, &service.UpdateAccountInput{ExpiryGraceMinutes: req.Minutes})
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"minutes": a.ExpiryGraceSeconds() / 60, "effective_expires_at": a.SchedulingExpiresAt()})
}

func (h *AccountHandler) GetAccountExpiryGrace(c *gin.Context)    { h.accountExpiryGrace(c, false) }
func (h *AccountHandler) UpdateAccountExpiryGrace(c *gin.Context) { h.accountExpiryGrace(c, true) }
