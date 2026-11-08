package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) GetBatchLifetimeStats(c *gin.Context) {
	var req BatchTodayStatsRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32*1024)
	if err := c.ShouldBindJSON(&req); err != nil || len(req.AccountIDs) > 1000 {
		response.BadRequest(c, "账号列表无效，单次最多 1000 个")
		return
	}
	stats, err := h.accountUsageService.GetLifetimeStatsBatch(c.Request.Context(), req.AccountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Never retain deleted-account statistics in a handler or browser cache.
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"stats": stats})
}
