package service

import (
	"encoding/json"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"strings"
	"time"
)

func IsExcelAuthorizationError(err error) bool {
	return strings.HasPrefix(infraerrors.Reason(err), "OPENAI_EXCEL_AUTH_")
}

func writeExcelBPSCompactFailure(c *gin.Context, status int, code, message, param string) {
	detail := map[string]string{"type": "invalid_request_error", "code": code, "message": message}
	if param != "" {
		detail["param"] = param
	}
	MarkOpsStreamError(c, code, message, status)
	body, _ := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": 0, "response": map[string]any{
		"id": "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "object": "response", "created_at": time.Now().Unix(), "status": "failed", "output": []any{}, "error": detail,
	}})
	_, _ = c.Writer.Write(append(append([]byte("event: response.failed\ndata: "), body...), []byte("\n\n")...))
	c.Writer.Flush()
}

func MarkOpsClientCancellation(c *gin.Context, stream bool) {
	MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: "client_canceled", Code: "client_canceled", Message: "Client disconnected: context canceled", IntendedStatus: 499, RequestScoped: true, NonStream: !stream})
}
