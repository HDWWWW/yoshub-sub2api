package admin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// External pool imports extend only the scheduling deadline, never token expiry.
// JWT-authenticated backup restores keep their original timestamps.
func applyExternalImportExpiry(req *DataImportRequest, authMethod string, grace ...int) {
	if authMethod != "admin_api_key" {
		return
	}
	for i := range req.Data.Accounts {
		a := &req.Data.Accounts[i]
		if a.Platform == service.PlatformOpenAI && a.Type == service.AccountTypeOAuth {
			minutes := service.AccountImportExpiryOffsetMinutes
			if len(grace) > 0 {
				minutes = grace[0]
			}
			a.ExpiresAt, a.Extra = service.ImportedAccountExpiryWithGrace(a.ExpiresAt, a.Extra, minutes)
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

func (h *AccountHandler) refillForRequest(c *gin.Context) (*service.AccountRefillService, bool) {
	if !h.refillAvailable(c) {
		return nil, false
	}
	id := int64(0)
	if value, present := c.GetQuery("group_id"); present {
		var err error
		id, err = strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "分组 ID 无效")
			return nil, false
		}
	}
	svc, err := h.accountRefill.ForGroup(c.Request.Context(), id)
	if err != nil {
		refillResponseError(c, err)
		return nil, false
	}
	return svc, true
}

func (h *AccountHandler) GetAccountRefill(c *gin.Context) {
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	view, err := svc.View(c.Request.Context())
	if err != nil {
		response.Error(c, 503, "补号状态读取失败")
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, view)
}

func (h *AccountHandler) UpdateAccountRefill(c *gin.Context) {
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	var req struct {
		Config   json.RawMessage `json:"config"`
		APIToken string          `json:"api_token"`
		Revision int64           `json:"revision"`
		Webhook  string          `json:"wecom_webhook"`
	}
	current, err := svc.View(c.Request.Context())
	if err != nil {
		response.Error(c, 503, "补号状态读取失败")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "补号设置格式无效")
		return
	}
	config, err := decodeAccountRefillConfig(current.Config, req.Config)
	if err != nil {
		response.BadRequest(c, "补号设置格式无效")
		return
	}
	if err := svc.Configure(c.Request.Context(), config, req.APIToken, req.Revision, req.Webhook); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func decodeAccountRefillConfig(current service.AccountRefillConfig, raw json.RawMessage) (service.AccountRefillConfig, error) {
	// Omitted fields from older clients retain their values. A provided plan map
	// replaces the old map so disabling an override actually removes it.
	if len(raw) == 0 {
		return current, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return current, err
	}
	if _, present := fields["plan_quota_thresholds"]; present {
		current.PlanQuotaThresholds = nil
	}
	if _, present := fields["plan_import_concurrency"]; present {
		current.PlanImportConcurrency = nil
	}
	err := json.Unmarshal(raw, &current)
	return current, err
}

func (h *AccountHandler) CheckAccountRefill(c *gin.Context) {
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	if err := svc.Check(ctx); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func (h *AccountHandler) RetryAccountRefill(c *gin.Context) {
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	if err := svc.Retry(c.Request.Context(), c.Param("job_id")); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}

func (h *AccountHandler) DownloadAccountRefill(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	middleware.SetAuditAction(c, "admin.accounts.refill.download")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 55*time.Second)
	defer cancel()
	data, err := svc.DownloadDelivery(ctx, c.Param("job_id"))
	if err != nil {
		var downloadErr *service.AccountRefillDownloadError
		if errors.As(err, &downloadErr) {
			if downloadErr.StatusCode == http.StatusTooManyRequests && downloadErr.RetryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(math.Ceil(downloadErr.RetryAfter.Seconds()))))
			}
			response.Error(c, downloadErr.StatusCode, downloadErr.Error())
		} else {
			response.Error(c, http.StatusBadGateway, "交付下载失败，请稍后重试")
		}
		return
	}
	c.Header("Content-Disposition", `attachment; filename="sub2api-refill.json"`)
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

func (h *AccountHandler) ManualAccountRefill(c *gin.Context) {
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	middleware.SetAuditAction(c, "admin.accounts.refill.manual")
	var req service.ManualAccountRefillRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "手动补号参数无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	if err := svc.Manual(ctx, req); err != nil {
		if errors.Is(err, service.ErrAccountRefillBusy) || service.IsManualRefillValidationError(err) {
			refillResponseError(c, err)
		} else {
			response.Error(c, 503, "请求状态暂无法确认，请使用原请求重试或刷新任务进度")
		}
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
	svc, ok := h.refillForRequest(c)
	if !ok {
		return
	}
	var req struct {
		ConfirmNotAccepted bool `json:"confirm_not_accepted"`
	}
	if c.ShouldBindJSON(&req) != nil || !req.ConfirmNotAccepted {
		response.BadRequest(c, "请先在号商处确认该请求未受理且没有订单，再释放本地预占")
		return
	}
	if err := svc.DismissRejected(c.Request.Context(), c.Param("job_id")); err != nil {
		refillResponseError(c, err)
		return
	}
	h.GetAccountRefill(c)
}
