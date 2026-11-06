package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) autoFastRequest(c *gin.Context) (int64, bool) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "自动Fast仅限完整管理员访问")
		return 0, false
	}
	if h.accountAutoFast == nil {
		response.Error(c, http.StatusServiceUnavailable, "自动Fast服务未就绪")
		return 0, false
	}
	id, err := strconv.ParseInt(c.Query("group_id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "请选择分组")
		return 0, false
	}
	c.Header("Cache-Control", "no-store")
	return id, true
}

func (h *AccountHandler) GetAccountAutoFast(c *gin.Context) {
	id, ok := h.autoFastRequest(c)
	if !ok {
		return
	}
	rule, err := h.accountAutoFast.View(c.Request.Context(), id)
	if err != nil {
		var invalid *service.AccountAutoFastConfigError
		if errors.As(err, &invalid) {
			response.BadRequest(c, invalid.Error())
		} else {
			response.Error(c, 503, "自动Fast状态读取失败")
		}
		return
	}
	response.Success(c, rule)
}

func (h *AccountHandler) UpdateAccountAutoFast(c *gin.Context) {
	id, ok := h.autoFastRequest(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req struct {
		Config   *service.AccountAutoFastConfig `json:"config"`
		Revision int64                          `json:"revision"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Config == nil || req.Revision < 0 {
		response.BadRequest(c, "自动Fast配置无效")
		return
	}
	if err := req.Config.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	err := h.accountAutoFast.Configure(c.Request.Context(), id, req.Revision, *req.Config)
	if err != nil {
		var invalid *service.AccountAutoFastConfigError
		if errors.Is(err, service.ErrAccountAutoFastBusy) || errors.Is(err, service.ErrAccountAutoFastConflict) {
			response.Error(c, 409, err.Error())
		} else if errors.As(err, &invalid) {
			response.BadRequest(c, invalid.Error())
		} else {
			response.Error(c, 503, "自动Fast配置保存失败，请稍后重试")
		}
		return
	}
	rule, err := h.accountAutoFast.View(c.Request.Context(), id)
	if err != nil {
		response.Error(c, 503, "已保存，请刷新状态")
		return
	}
	response.Success(c, rule)
}
