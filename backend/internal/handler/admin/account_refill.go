package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// External pool imports extend only the scheduling deadline, never token expiry.
// JWT-authenticated backup restores keep their original timestamps.
func applyExternalImportExpiry(req *DataImportRequest, authMethod string) {
	if authMethod != "admin_api_key" {
		return
	}
	for i := range req.Data.Accounts {
		a := &req.Data.Accounts[i]
		if a.Platform == service.PlatformOpenAI && a.Type == service.AccountTypeOAuth {
			a.ExpiresAt, a.Extra = service.ImportedAccountExpiry(a.ExpiresAt, a.Extra)
		}
	}
}

func (h *AccountHandler) refillAvailable(c *gin.Context) bool {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "补号设置仅限完整管理员访问")
		return false
	}
	if h.accountRefill == nil {
		response.Error(c, http.StatusServiceUnavailable, "补号服务未就绪")
		return false
	}
	return true
}

func (h *AccountHandler) GetAccountRefill(c *gin.Context) {
	if !h.refillAvailable(c) {
		return
	}
	view, err := h.accountRefill.View(c.Request.Context())
	if err != nil {
		response.Error(c, 503, "补号状态读取失败")
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, view)
}

func (h *AccountHandler) UpdateAccountRefill(c *gin.Context) {
	if !h.refillAvailable(c) {
		return
	}
	var req struct {
		Config   service.AccountRefillConfig `json:"config"`
		APIToken string                      `json:"api_token"`
		Revision int64                       `json:"revision"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "补号设置格式无效")
		return
	}
	if err := h.accountRefill.Configure(c.Request.Context(), req.Config, req.APIToken, req.Revision); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func (h *AccountHandler) CheckAccountRefill(c *gin.Context) {
	if !h.refillAvailable(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	if err := h.accountRefill.Check(ctx); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func (h *AccountHandler) RetryAccountRefill(c *gin.Context) {
	if !h.refillAvailable(c) {
		return
	}
	if err := h.accountRefill.Retry(c.Request.Context(), c.Param("job_id")); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func refillResponseError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrAccountRefillBusy) {
		response.Error(c, 409, err.Error())
		return
	}
	// Service validation errors contain no provider body or credentials.
	response.BadRequest(c, err.Error())
}

func (h *AccountHandler) DismissAccountRefill(c *gin.Context) {
	if !h.refillAvailable(c) {
		return
	}
	var req struct {
		ConfirmNotAccepted bool `json:"confirm_not_accepted"`
	}
	if c.ShouldBindJSON(&req) != nil || !req.ConfirmNotAccepted {
		response.BadRequest(c, "请先在号商处确认该请求未受理且没有订单，再释放本地预占")
		return
	}
	if err := h.accountRefill.DismissRejected(c.Request.Context(), c.Param("job_id")); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}
