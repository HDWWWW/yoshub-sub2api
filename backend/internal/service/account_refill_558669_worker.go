package service

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"
)

// Keep reconciliation bounded and fair across new deliveries and imported
// accounts awaiting entitlement confirmation. Neither path creates a new order.
func (s *AccountRefillService) advance558669Jobs(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	if state.Supplier558669.EncryptedToken == "" || now.Before(state.Supplier558669.RetryAt) {
		return nil
	}
	selected := -1
	for i, j := range state.Jobs {
		if j.Provider != refillProvider558669 || now.Before(j.NextAttemptAt) {
			continue
		}
		if !j.needs558669Sync(now) && (!j.pending() || j.Status == "blocked" || j.Status == "import_failed" || (!state.Config.PushEnabled && j.Status == "awaiting_push")) {
			continue
		}
		if selected < 0 || j.NextAttemptAt.Before(state.Jobs[selected].NextAttemptAt) {
			selected = i
		}
	}
	if selected < 0 {
		return nil
	}
	return s.advance558669(ctx, state, &state.Jobs[selected], persist)
}

func (s *AccountRefillService) advance558669(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if time.Now().Before(state.Supplier558669.RetryAt) {
		return nil
	}
	token, err := s.encryptor.Decrypt(state.Supplier558669.EncryptedToken)
	if err != nil || !valid558669Token(token) {
		return errors.New("558669 密钥不可用，请重新配置")
	}
	if err := s.notifyRefillStarted(ctx, state, j, persist); err != nil {
		return err
	}
	client := s.providerClient(refillProvider558669)
	now := time.Now()
	imported := j.Status == "imported"
	j.UpdatedAt = now
	if !imported && j.ResultReady && refill558669ReadyStatus(j.SupplierStatus) {
		return s.import558669(ctx, token, state, j, persist)
	}
	if j.SupplierID == "" && j.SubmissionStartedAt == nil {
		// A queued reservation may be old. Refresh its quote before the first
		// POST only; all subsequent attempts keep this exact payload and key.
		if err := s.refresh558669(ctx, state, persist); err != nil {
			return err
		}
		var quote *RefillQuote
		for i := range state.Supplier558669.Catalog.Items {
			q := &state.Supplier558669.Catalog.Items[i]
			if q.Service == "refill" && q.Plan == j.Plan && q.Enabled {
				quote = q
				break
			}
		}
		day := j.CreatedAt.In(refillTimezone).Format("2006-01-02")
		u := state.Supplier558669.Usage[day]
		held, err := s.reserved558669(ctx, state)
		if err != nil {
			return err
		}
		if quote == nil || quote.Price > state.Config.limits558669().MaxUnitPrice ||
			u.Points-j.Price+quote.Price > state.Config.limits558669().DailyMaxPoints+1e-8 ||
			state.Supplier558669.Catalog.Available+1e-8 < held-j.Price+quote.Price {
			j.Status, j.Error = "blocked", "558669 最新报价不可用或超过预算，请核对原任务后重试"
			return persist()
		}
		u.Points += quote.Price - j.Price
		state.Supplier558669.Usage[day] = u
		j.Price, j.PriceVersion = quote.Price, quote.PriceVersion
		j.SubmissionStartedAt = &now
		if err := persist(); err != nil {
			return err
		}
	}
	var remote *RefillProviderJob
	if j.SupplierID == "" {
		remote, err = client.Submit(ctx, token, *j)
	} else {
		remote, err = client.Status(ctx, token, j.SupplierID)
	}
	j.Attempts++
	if err != nil {
		return s.fail558669(state, j, err, imported, persist)
	}
	if j.SupplierID == "" && remote != nil && valid558669ID(remote.ID) {
		// Even an inconsistent response may identify an accepted order. Retain
		// its ID for read-only reconciliation; never adopt a different ID later.
		j.SupplierID = remote.ID
	}
	if err := validate558669Job(remote, *j); err != nil {
		if !imported {
			j.Status = "supplier_unknown"
		}
		j.ResultReady, j.Error = false, err.Error()
		j.NextAttemptAt = time.Now().Add(120 * time.Second)
		return persist()
	}
	j.SupplierID, j.SupplierStatus, j.ResultReady = remote.ID, remote.Status, remote.ResultReady
	j.EntitlementStatus, j.BillingStatus = remote.EntitlementStatus, remote.BillingStatus
	j.SupplierEmail, j.DeliveryVersion = remote.Email, remote.DeliveryVersion
	if remote.ExpiresAt != nil {
		j.SupplierExpiresAt = remote.ExpiresAt
		// Once imported, preserve the original rental deadline and admin expiry.
		if !imported {
			j.ExpiresAt = remote.ExpiresAt
		}
	}
	j.Error, j.SupplierError = "", ""
	j.NextAttemptAt = nextRefillOrderPoll(nil)
	if imported {
		j.NextAttemptAt = time.Now().Add(120 * time.Second)
		if remote.EntitlementStatus == "confirmed" {
			j.NextAttemptAt = time.Now().Add(5 * time.Minute)
		}
		if remote.Status == "failed" {
			j.CredentialsSyncStopped = true
			j.Error = "558669 已导入任务转为失败，请核对原账号和计费状态"
			return persist()
		}
		if remote.ResultReady && refill558669ReadyStatus(remote.Status) && remote.DeliveryVersion > j.AppliedDeliveryVersion {
			delivery, err := client.Delivery(ctx, token, j.SupplierID)
			if err != nil {
				return s.fail558669(state, j, err, true, persist)
			}
			syncer, ok := s.admin.(interface {
				Sync558669Delivery(context.Context, AccountRefillJob, *RefillDelivery) error
			})
			if !ok {
				j.Error = "558669 凭据更新服务暂不可用"
				return persist()
			}
			if err := syncer.Sync558669Delivery(ctx, *j, delivery); err != nil {
				j.Error = "558669 新版凭据核对失败，保留原账号并继续核对"
			} else {
				j.AppliedDeliveryVersion = remote.DeliveryVersion
			}
		}
		return persist()
	}
	j.Status = remote.Status
	if !refillProviderStatusKnown(remote.Status) {
		j.Status, j.ResultReady, j.Error = "supplier_unknown", false, "558669 返回未知状态，保留原任务核对，不重复购买"
	} else if remote.Status == "failed" {
		j.Error, j.FinishedAt = "558669 已确认补号失败，释放本地积分预占", &now
		state.release558669Budget(j)
		markRefillFailureReplacement(state, j)
		if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
			return err
		}
		return persist()
	} else if remote.Status == "review" {
		j.Error = "558669 原任务待核对，继续查询原任务，不重复购买"
		j.NextAttemptAt = time.Now().Add(120 * time.Second)
	} else if remote.Status == "succeeded" {
		j.Status = "awaiting_delivery"
	}
	if refill558669ReadyStatus(remote.Status) && refillDeliveryExpired(j.ExpiresAt, now) {
		finishRefillDeliveryExpired(j, now)
		return persist()
	}
	if err := persist(); err != nil {
		return err
	}
	if remote.ResultReady && refill558669ReadyStatus(remote.Status) {
		return s.import558669(ctx, token, state, j, persist)
	}
	return nil
}

func refill558669ReadyStatus(status string) bool {
	return status == "queued" || status == "processing" || status == "succeeded" || status == "review"
}

func (s *AccountRefillService) import558669(ctx context.Context, token string, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if !state.Config.PushEnabled {
		j.Status, j.NextAttemptAt = "awaiting_push", time.Now().Add(time.Minute)
		return persist()
	}
	j.Status = "importing"
	if err := persist(); err != nil {
		return err
	}
	if err := s.importDelivery(ctx, token, j); err != nil {
		return s.fail558669(state, j, err, false, persist)
	}
	now := time.Now()
	j.Status, j.Error, j.FinishedAt = "imported", "", &now
	j.NextAttemptAt = now.Add(120 * time.Second)
	completeRefillExpiryDemand(state, *j)
	state.Message = "558669 新账号已导入，继续核对原任务的权益与凭据版本"
	if err := s.notifyRefillImported(ctx, state, j, persist); err != nil {
		return err
	}
	return persist()
}

func (s *AccountRefillService) fail558669(state *accountRefillState, j *AccountRefillJob, err error, imported bool, persist func() error) error {
	j.NextAttemptAt = nextRefillOrderPoll(err)
	j.Error = "558669 原任务暂未完成，继续核对原请求，不新建替代订单"
	var remote *refillProviderError
	if errors.As(err, &remote) {
		j.Error = remote.Error()
		if remote.code == 429 || remote.code == 401 || remote.code == 403 {
			state.Supplier558669.RetryAt = j.NextAttemptAt
		}
		if remote.code == 410 {
			if imported {
				j.CredentialsSyncStopped = true
			} else {
				finishRefillDeliveryExpired(j, time.Now())
			}
		} else if !imported && remote.code >= 400 && remote.code < 500 && remote.code != 429 && remote.code != 409 {
			j.Status = "blocked"
		}
	}
	if !imported && j.Status == "importing" {
		j.Status = "import_failed"
		var temporary *refillProviderTemporaryError
		if errors.As(err, &temporary) || remote != nil && (remote.code == 409 || remote.code == 429 || remote.code >= 500) {
			j.Status, j.ResultReady = "awaiting_delivery", false
			j.NextAttemptAt = maxTime(j.NextAttemptAt, time.Now().Add(120*time.Second))
		}
	}
	if errors.Is(err, errRefillDeliveryExpired) {
		finishRefillDeliveryExpired(j, time.Now())
	}
	state.Message = j.Error
	return persist()
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Apply only supplier-owned credential fields in the same transaction as the
// version marker. Existing account settings, proxy, groups and lease stay intact.
func (s *adminServiceImpl) Sync558669Delivery(ctx context.Context, j AccountRefillJob, d *RefillDelivery) error {
	if j.Provider != refillProvider558669 || j.Status != "imported" || j.AccountID <= 0 {
		return errors.New("558669 账号尚未导入")
	}
	if err := validateRefillDelivery(d); err != nil {
		return err
	}
	writer, ok := s.accountRepo.(interface {
		UpdateCredentials(context.Context, int64, map[string]any) error
	})
	if !ok {
		return errors.New("凭据更新存储不可用")
	}
	_, err := s.scopedAdminTransaction(ctx, j.AccountID, false, func(txctx context.Context) (*Account, error) {
		a, err := s.accountRepo.GetByID(txctx, j.AccountID)
		if err != nil {
			return nil, err
		}
		credentials, err := refreshed558669Credentials(a, j, d)
		if err != nil || credentials == nil {
			return a, err
		}
		if err := writer.UpdateCredentials(txctx, a.ID, credentials); err != nil {
			return nil, err
		}
		if err := s.accountRepo.UpdateExtra(txctx, a.ID, map[string]any{"refill_delivery_version": j.DeliveryVersion}); err != nil {
			return nil, err
		}
		return s.accountRepo.GetByID(txctx, a.ID)
	})
	return err
}

func refreshed558669Credentials(a *Account, j AccountRefillJob, d *RefillDelivery) (map[string]any, error) {
	if a == nil || !a.IsOpenAIOAuth() || a.IsShadow() || a.Extra["refill_job_id"] != j.ID || a.Extra["refill_supplier_id"] != j.SupplierID || a.Extra["import_source"] != "558669_api" {
		return nil, errors.New("558669 账号与原任务不匹配")
	}
	incoming := d.Accounts[0].Credentials
	email, _ := a.Credentials["email"].(string)
	nextEmail, _ := incoming["email"].(string)
	if email == "" || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(nextEmail)) {
		return nil, errors.New("558669 交付邮箱发生变化")
	}
	for _, key := range []string{"chatgpt_account_id", "chatgpt_user_id", "organization_id"} {
		if old, ok := a.Credentials[key].(string); ok && old != "" && incoming[key] != old {
			return nil, errors.New("558669 交付身份发生变化")
		}
	}
	if j.DeliveryVersion <= refill558669Version(a.Extra) {
		return nil, nil
	}
	merged := maps.Clone(a.Credentials)
	for _, key := range []string{"access_token", "refresh_token", "id_token", "client_id", "expires_at", "plan_type"} {
		if value, ok := incoming[key].(string); ok && value != "" {
			merged[key] = value
		}
	}
	if value, ok := incoming["expires_at"].(float64); ok {
		merged["expires_at"] = value
	}
	return merged, nil
}
