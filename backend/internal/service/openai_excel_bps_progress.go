package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

func (s *OpenAIGatewayService) guardExcelBPSProgress(ctx context.Context, response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	idle := 120 * time.Second
	if s.cfg != nil {
		idle = excelBPSProgressTimeout(s.cfg.Gateway.ExcelBPSStreamDataIntervalTimeout)
	}
	response.Body = basispoints.WithProgressTimeout(ctx, response.Body, idle)
}
