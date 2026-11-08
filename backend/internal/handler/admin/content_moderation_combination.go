package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ContentModerationHandler) TestCombinationRules(c *gin.Context) {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Error(c, 403, "组合规则测试仅限完整管理员访问")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)
	var req struct {
		Rules []service.ContentModerationCombinationRule `json:"rules"`
		Text  string                                     `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	hits, err := service.TestContentModerationCombinationRules(req.Rules, req.Text)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"matches": hits})
}
