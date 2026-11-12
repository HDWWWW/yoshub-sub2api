package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/gin-gonic/gin"
)

const SettingKeyExcelBPSEnabled = "excel_bps_enabled"
const HTTPUpstreamProfileExcelBPS = HTTPUpstreamProfileLongStream

var errExcelBPSProxyUnavailable = errors.New("BPS account proxy unavailable")

type excelBPSForwardError struct{ code string }

func (e *excelBPSForwardError) Error() string { return "excel BPS: " + e.code }

type excelBPSAcquisitionFailure struct{ cause error }

func (e *excelBPSAcquisitionFailure) Error() string { return errExcelBPSProxyUnavailable.Error() }
func (e *excelBPSAcquisitionFailure) Unwrap() []error {
	return []error{errExcelBPSProxyUnavailable, e.cause}
}

type excelBPSLease interface {
	Release()
	ReportFailure()
	ReportStreamFailure()
	ReportSuccess()
	ReportUpstreamFailure()
}
type excelBPSAcquire func(context.Context, string, ...string) (string, excelBPSLease, error)

// Imported pool settings must not silently fall through to a direct connection.
// This integration uses the existing account proxy lifecycle.
func (a *Account) IsExcelBPSMihomoEnabled() bool {
	return a.IsExcelBPSEnabled() && a.Extra["openai_excel_bps_mihomo"] == true
}
func (s *OpenAIGatewayService) excelBPSAcquireFor(_ *Account) excelBPSAcquire { return nil }

func (s *OpenAIGatewayService) excelBPSGloballyEnabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if s.settingService == nil || s.settingService.settingRepo == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayForwardingDBTimeout)
	defer cancel()
	v, err := s.settingService.settingRepo.GetValue(ctx, SettingKeyExcelBPSEnabled)
	return errors.Is(err, ErrSettingNotFound) || (err == nil && v != "false")
}

func (s *OpenAIGatewayService) getExcelBPSAccessToken(ctx context.Context, account *Account) (string, error) {
	if !account.IsExcelOAuth() {
		return "", infraerrors.ServiceUnavailable("OPENAI_EXCEL_AUTH_MISSING", "Excel BPS requires a separately authorized Excel OAuth account; a Codex token cannot be used")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	return token, err
}

func (s *OpenAIGatewayService) handleExcelBPSUnauthorized(ctx context.Context, account *Account, status int, headers http.Header, _ []byte, _ ...string) {
	if status != http.StatusUnauthorized || !account.IsExcelOAuth() || s.rateLimitService == nil {
		return
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	s.rateLimitService.HandleUpstreamError(stateCtx, account, status, headers, []byte(`{"error":{"message":"Excel BPS authentication failed"}}`))
}

func (s *OpenAIGatewayService) doExcelBPSHTTP(req *http.Request, proxy string, account *Account) (*http.Response, error) {
	if err := openAIAccountContextError(req.Context(), account); err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileExcelBPS)))
	return s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
}

func (s *OpenAIGatewayService) doExcelBPSRequest(ctx context.Context, c *gin.Context, account *Account, scope string, body []byte, token, accountID string, acquire excelBPSAcquire) (*http.Response, excelBPSLease, string, error) {
	resp, lease, proxy, err := s.doExcelBPSRequestTo(ctx, c, account, scope, basispoints.ResponsesURL, func(ctx context.Context) (*http.Request, error) {
		return newExcelBPSRequest(ctx, body, token, accountID)
	}, acquire)
	if err == nil {
		s.guardExcelBPSProgress(ctx, resp)
	}
	return resp, lease, proxy, err
}

// A transport error does not prove a generation was unsent. Never replay it.
func (s *OpenAIGatewayService) doExcelBPSRequestTo(ctx context.Context, c *gin.Context, account *Account, scope, upstreamURL string, build func(context.Context) (*http.Request, error), _ excelBPSAcquire) (*http.Response, excelBPSLease, string, error) {
	proxy := ""
	if account.IsExcelBPSMihomoEnabled() || (account.ProxyID != nil && account.Proxy == nil) {
		return nil, nil, "", errExcelBPSProxyUnavailable
	}
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, proxy, err
	}
	req, err := build(ctx)
	if err != nil {
		return nil, nil, proxy, err
	}
	c.Set("excel_bps_upstream_attempt", 1)
	resp, err := s.doExcelBPSHTTP(req, proxy, account)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		recordExcelBPSTransportFailureAt(ctx, c, account, upstreamURL, scope, proxy, err, "transport", 1, false)
		return nil, nil, proxy, err
	}
	return resp, nil, proxy, nil
}

func recordExcelBPSTransportFailure(ctx context.Context, c *gin.Context, account *Account, scope, proxy string, err error, stage string, attempt int, retry bool) {
	recordExcelBPSTransportFailureAt(ctx, c, account, basispoints.ResponsesURL, scope, proxy, err, stage, attempt, retry)
}
func recordExcelBPSTransportFailureAt(_ context.Context, c *gin.Context, account *Account, upstreamURL, _, _ string, err error, stage string, _ int, _ bool) {
	if isExcelBPSClientCancellation(c, err) {
		return
	}
	kind := transportdiag.Classify(err)
	if errors.Is(err, errExcelBPSProxyUnavailable) {
		kind = "proxy_unavailable"
	}
	message := "Excel BPS " + stage + " failed: " + kind
	setOpsUpstreamError(c, 0, message, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: account.Platform, AccountID: account.ID, UpstreamURL: upstreamURL, Kind: "request_error", Stage: stage, Scope: "excel_bps", Reason: kind, Message: message})
}

type excelBPSBorrowedLease struct{ excelBPSLease }

func (excelBPSBorrowedLease) Release() {}
func pinnedExcelBPSAcquire(proxy string, lease excelBPSLease) excelBPSAcquire {
	return func(ctx context.Context, _ string, excluded ...string) (string, excelBPSLease, error) {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if len(excluded) > 0 {
			return "", nil, errExcelBPSProxyUnavailable
		}
		return proxy, excelBPSBorrowedLease{lease}, nil
	}
}

// Keep timer input in seconds bounded even for partially populated test configs.
func excelBPSProgressTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}
