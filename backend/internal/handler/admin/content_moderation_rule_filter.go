package admin

import (
	"encoding/json"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ContentModerationHandler) RuleFilterCatalog(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "规则管理仅限完整管理员访问")
		return
	}
	response.Success(c, service.ContentModerationRuleCatalog())
}

func (h *ContentModerationHandler) TestRuleFilter(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "规则测试仅限完整管理员访问")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)
	var req struct {
		Config   *service.ContentModerationRuleFilter `json:"config"`
		Text     string                               `json:"text"`
		Body     json.RawMessage                      `json:"body"`
		Protocol string                               `json:"protocol"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := service.TestContentModerationRuleFilter(c.Request.Context(), req.Config, req.Text, req.Body, req.Protocol)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
