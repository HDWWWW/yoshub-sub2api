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

func (h *AccountHandler) riskRequest(c *gin.Context) (int64, bool) {
	if adminaccess.FromContext(c.Request.Context()) != nil {
		response.Error(c, http.StatusForbidden, "风控检测仅限完整管理员访问")
		return 0, false
	}
	if h.accountRisk == nil {
		response.Error(c, http.StatusServiceUnavailable, "风控检测服务未就绪")
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
func (h *AccountHandler) GetAccountRiskDetection(c *gin.Context) {
	id, ok := h.riskRequest(c)
	if !ok {
		return
	}
	view, err := h.accountRisk.View(c.Request.Context(), id)
	if err != nil {
		response.Error(c, 503, "风控检测状态读取失败")
		return
	}
	response.Success(c, view)
}
func (h *AccountHandler) UpdateAccountRiskDetection(c *gin.Context) {
	id, ok := h.riskRequest(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 24<<10)
	var req struct {
		Config   *service.AccountRiskConfig `json:"config"`
		Revision int64                      `json:"revision"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Config == nil {
		response.BadRequest(c, "风控检测配置无效")
		return
	}
	if err := req.Config.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	err := h.accountRisk.Configure(c.Request.Context(), id, req.Revision, *req.Config)
	if err != nil {
		if errors.Is(err, service.ErrAccountRiskBusy) || errors.Is(err, service.ErrAccountRiskConflict) {
			response.Error(c, 409, err.Error())
		} else {
			response.Error(c, 503, "配置保存失败，请检查分组状态后重试")
		}
		return
	}
	view, err := h.accountRisk.View(c.Request.Context(), id)
	if err != nil {
		response.Error(c, 503, "配置已保存，状态读取失败，请刷新")
		return
	}
	response.Success(c, view)
}
