package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetSmartWSMetrics returns only bounded process-level aggregates.
func (h *OpsHandler) GetSmartWSMetrics(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	snapshot, err := h.opsService.GetSmartWSMetrics(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, snapshot)
}
