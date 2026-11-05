package service

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
	"time"
)

type bpsTestUpstream struct {
	httpUpstreamRecorder
	send func(*http.Request, string) (*http.Response, error)
}

func (u *bpsTestUpstream) Do(r *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	return u.send(r, proxy)
}

type excelBPSQuotaWrite struct {
	accountID int64
	updates   map[string]any
	resetAt   time.Time
	ctxErr    error
	deadline  time.Time
}

type excelBPSQuotaRepo struct {
	AccountRepository
	writes    chan excelBPSQuotaWrite
	updateErr error
	limitErr  error
}

type excelBPSQuotaSettingsRepo struct {
	SettingRepository
	cooldown string
}

func (r *excelBPSQuotaSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == SettingKeyRateLimit429CooldownSettings {
		return r.cooldown, nil
	}
	return "", ErrSettingNotFound
}

func (r *excelBPSQuotaRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	copied := make(map[string]any, len(updates))
	for key, value := range updates {
		copied[key] = value
	}
	deadline, _ := ctx.Deadline()
	r.writes <- excelBPSQuotaWrite{accountID: id, updates: copied, ctxErr: ctx.Err(), deadline: deadline}
	return r.updateErr
}

func (r *excelBPSQuotaRepo) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	deadline, _ := ctx.Deadline()
	r.writes <- excelBPSQuotaWrite{accountID: id, resetAt: resetAt, ctxErr: ctx.Err(), deadline: deadline}
	return r.limitErr
}

func nextExcelBPSQuotaWrite(t *testing.T, repo *excelBPSQuotaRepo) excelBPSQuotaWrite {
	t.Helper()
	select {
	case write := <-repo.writes:
		return write
	case <-time.After(3 * time.Second):
		t.Fatal("expected an account quota update")
		return excelBPSQuotaWrite{}
	}
}

func requireNoExcelBPSQuotaWrite(t *testing.T, repo *excelBPSQuotaRepo) {
	t.Helper()
	select {
	case write := <-repo.writes:
		t.Fatalf("unexpected account quota update: %+v", write)
	case <-time.After(20 * time.Millisecond):
	}
}

func excelBPSQuotaHeaders(used5h, used7d string) http.Header {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", used7d)
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", used5h)
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")
	return headers
}

func requireExcelBPSRateLimitFailover(t *testing.T, err error, c *gin.Context) {
	t.Helper()
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
	require.Equal(t, ExcelBPSRateLimitedReason, failover.Reason)
	require.Equal(t, http.StatusTooManyRequests, failover.ClientStatusCode)
	require.True(t, failover.ShouldRetryNextAccount())
	require.False(t, failover.RetryableOnSameAccount, "do not retry the throttled account in this request")
	require.False(t, failover.ShouldReportAccountScheduleFailure(), "keep the shared scheduler score")
	require.Empty(t, failover.ResponseBody)
	for key := range failover.ResponseHeaders {
		require.Equal(t, "Retry-After", key)
	}
	require.False(t, c.Writer.Written(), "leave the response to the handler")
	require.False(t, IsResponseCommitted(c))
}
