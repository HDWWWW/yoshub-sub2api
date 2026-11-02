package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type AccountRefillDownloadError struct {
	StatusCode int
	RetryAfter time.Duration
	message    string
}

func (e *AccountRefillDownloadError) Error() string { return e.message }

func refillDownloadError(code int, message string) error {
	return &AccountRefillDownloadError{StatusCode: code, message: message}
}

func refillDownloadProviderError(err error) error {
	var remote *refillProviderError
	if errors.As(err, &remote) {
		code := http.StatusBadGateway
		// Supplier authentication errors must not log the Sub2 administrator out.
		if remote.code == 409 || remote.code == 410 || remote.code == 429 {
			code = remote.code
		}
		message := remote.Error()
		if remote.code == 409 {
			message = "号商交付尚未就绪，请稍后刷新原任务再下载"
		}
		return &AccountRefillDownloadError{StatusCode: code, RetryAfter: remote.retryAfter, message: message}
	}
	return refillDownloadError(http.StatusBadGateway, "号商交付读取失败，请稍后重试原任务")
}

func (s *AccountRefillService) handleDownloadProviderError(ctx context.Context, err error) error {
	var remote *refillProviderError
	if errors.As(err, &remote) && remote.code == http.StatusTooManyRequests {
		deadline := time.Now().Add(refillRetryDelay(err, 1))
		// Merge only the backoff into the latest state. Downloads run outside the
		// worker lock; writing their earlier snapshot would lose order progress.
		saveErr := withAccountRefillConfigLock(ctx, s.store, 30*time.Second, func(raw string, save func(string) error) error {
			state, decodeErr := decodeAccountRefillState(raw)
			if decodeErr != nil {
				return decodeErr
			}
			if deadline.After(state.ProviderRetryAt) {
				state.ProviderRetryAt = deadline
				return saveRefillState(&state, save)
			}
			return nil
		})
		if saveErr != nil {
			return &AccountRefillDownloadError{StatusCode: http.StatusTooManyRequests, RetryAfter: refillRetryDelay(err, 1),
				message: "号商请求限流，退避保存失败，请等待 Retry-After 指定时间后重试"}
		}
		return &AccountRefillDownloadError{StatusCode: http.StatusTooManyRequests, RetryAfter: refillRetryDelay(err, 1),
			message: "号商请求限流，已暂停当前规则的号商请求，请稍后重试"}
	}
	return refillDownloadProviderError(err)
}

// DownloadDelivery only reads the selected rule's existing order. It never
// submits, imports, releases budget, or stores the returned OAuth credentials.
func (s *AccountRefillService) DownloadDelivery(ctx context.Context, id string) (json.RawMessage, error) {
	raw, err := s.store.Read(ctx)
	if err != nil {
		return nil, refillDownloadError(http.StatusServiceUnavailable, "补号状态读取失败")
	}
	state, err := decodeAccountRefillState(raw)
	if err != nil {
		return nil, refillDownloadError(http.StatusServiceUnavailable, "补号状态读取失败")
	}
	var job *AccountRefillJob
	for i := range state.Jobs {
		if state.Jobs[i].ID == id {
			job = &state.Jobs[i]
			break
		}
	}
	if job == nil {
		return nil, refillDownloadError(http.StatusNotFound, "当前分组没有此补号任务")
	}
	if job.Status == "delivery_expired" || refillDeliveryExpired(job.ExpiresAt, time.Now()) {
		return nil, refillDownloadError(http.StatusGone, "号商交付已过租期，无法下载 JSON")
	}
	if job.SupplierID == "" || job.SupplierStatus != "succeeded" || !job.ResultReady {
		return nil, refillDownloadError(http.StatusConflict, "补号尚未成功或交付 JSON 尚未就绪")
	}
	if delay := time.Until(state.ProviderRetryAt); delay > 0 {
		return nil, &AccountRefillDownloadError{StatusCode: http.StatusTooManyRequests, RetryAfter: delay, message: "号商正在退避，请稍后重试下载"}
	}
	if state.EncryptedToken == "" {
		return nil, refillDownloadError(http.StatusBadRequest, "请先配置号商 API Key")
	}
	token, err := s.encryptor.Decrypt(state.EncryptedToken)
	if err != nil {
		return nil, refillDownloadError(http.StatusBadRequest, "密钥解密失败，请重新保存 API Key")
	}
	remote, err := s.provider.Status(ctx, token, job.SupplierID)
	if err != nil {
		return nil, s.handleDownloadProviderError(ctx, err)
	}
	if err := validateRefillProviderJob(remote, *job); err != nil {
		return nil, refillDownloadError(http.StatusBadGateway, err.Error())
	}
	if refillDeliveryExpired(remote.ExpiresAt, time.Now()) {
		return nil, refillDownloadError(http.StatusGone, "号商交付已过租期，无法下载 JSON")
	}
	if remote.Status != "succeeded" || !remote.ResultReady {
		return nil, refillDownloadError(http.StatusConflict, "号商尚未确认交付就绪，请刷新原任务后重试")
	}
	delivery, err := s.provider.Delivery(ctx, token, job.SupplierID)
	if err != nil {
		return nil, s.handleDownloadProviderError(ctx, err)
	}
	if err := validateRefillDelivery(delivery); err != nil {
		return nil, refillDownloadError(http.StatusBadGateway, err.Error())
	}
	// The interactive SUB2API importer requires a proxies array, even when empty.
	if delivery.Proxies == nil {
		return nil, refillDownloadError(http.StatusBadGateway, "号商交付 JSON 缺少有效的代理列表")
	}
	if refillDeliveryExpired(delivery.Accounts[0].ExpiresAt, time.Now()) {
		return nil, refillDownloadError(http.StatusGone, "号商交付已过租期，无法下载 JSON")
	}
	data, err := json.Marshal(delivery)
	if err != nil {
		return nil, refillDownloadError(http.StatusBadGateway, "号商交付 JSON 无效")
	}
	return data, nil
}
