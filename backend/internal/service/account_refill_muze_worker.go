package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const muzeStudioReview = "Muze 工作室订单回执待核对，请到号商「我的订单」确认原订单 ID 后关联"

func (j AccountRefillJob) awaitingStudioReceipt() bool {
	return j.Provider == refillProviderMuze && j.MuzeStudio && j.SupplierID == "" && j.SubmissionStartedAt != nil
}

func validateMuzeCycle(c *muzeCycle, j AccountRefillJob) error {
	if c == nil || !validMuzeIdentifier(c.ID, false) || j.SubmissionStartedAt == nil || j.MuzeConnectionID == "" || (j.SupplierID != "" && c.ID != j.SupplierID) || (c.RequestID != "" && c.RequestID != j.ID) || (c.Product != "" && c.Product != j.muzeSKU()) {
		return errors.New("muze 订单编号、请求或套餐与本任务不匹配")
	}
	// Before a supplier ID is bound, only a matching request ID is proof of ownership.
	if j.SupplierID == "" && c.RequestID != j.ID {
		return errors.New("muze 订单缺少匹配的请求编号")
	}
	if (c.PushProvider != "" && c.PushProvider != "sub2api") || (c.PushConfigID != "" && c.PushConfigID != j.MuzeConnectionID) {
		return errors.New("muze 订单的推送连接与本任务不匹配")
	}
	if c.CreatedAt != "" {
		created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil || created.Before(j.SubmissionStartedAt.Add(-30*time.Second)) {
			return errors.New("muze 订单时间与本次下单不匹配")
		}
	}
	return nil
}

func (s *AccountRefillService) advanceMuzeJobs(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	index := -1
	for i, j := range state.Jobs {
		recoverable := j.SupplierID == "" && j.SubmissionStartedAt != nil && !j.MuzeStudio
		if j.Provider != refillProviderMuze || !j.pending() || now.Before(j.NextAttemptAt) || (j.Status == "blocked" && !recoverable) || j.Status == "import_failed" || (!state.Config.PushEnabled && j.Status == "awaiting_push") {
			continue
		}
		if index < 0 || j.NextAttemptAt.Before(state.Jobs[index].NextAttemptAt) {
			index = i
		}
	}
	if index < 0 {
		return nil
	}
	return s.advanceMuzeJob(ctx, state, &state.Jobs[index], now, persist)
}

func (s *AccountRefillService) advanceMuzeJob(ctx context.Context, state *accountRefillState, j *AccountRefillJob, now time.Time, persist func() error) error {
	token, err := s.encryptor.Decrypt(state.Muze.EncryptedToken)
	if err != nil {
		j.Status, j.Error = "blocked", "muze对接码解密失败，请重新保存"
		return persist()
	}
	if err := s.notifyRefillStarted(ctx, state, j, persist); err != nil {
		return err
	}
	j.UpdatedAt = now
	if j.SupplierID == "" {
		if j.SubmissionStartedAt != nil {
			if j.MuzeStudio {
				j.Status, j.Error = "blocked", muzeStudioReview
				return persist()
			}
			cycle, err := s.recoverMuzeOrder(ctx, token, state, *j)
			j.Attempts++
			if err != nil {
				j.Status, j.Error = "blocked", "muze尚未确认唯一交付，正在只读核对原订单；不会重复下单"
				j.NextAttemptAt = nextRefillOrderPoll(err)
				var remote *refillProviderError
				if errors.As(err, &remote) && remote.code == 429 {
					state.Muze.RetryAt = j.NextAttemptAt
				}
				return persist()
			}
			if expires := cycle.expiresAt(); expires != nil {
				j.SupplierExpiresAt = expires
			}
			j.SupplierID, j.Status, j.Error, j.SupplierError = cycle.ID, "processing", "", ""
			state.Muze.CheckedAt = nil
			if err := persist(); err != nil {
				return err
			}
		} else {
			j.SubmissionStartedAt, j.Status = &now, "submitting"
			j.Attempts++
			if err := persist(); err != nil {
				return err
			}
			order, err := s.muzeClient().Order(ctx, token, *j)
			var rejection *muzeHTTPError
			if errors.As(err, &rejection) && rejection.rejection {
				j.Status, j.SupplierStatus, j.SupplierError = "failed", "rejected", rejection.Error()
				j.Error, j.FinishedAt, j.BudgetReleased = "muze 拒绝下单："+j.SupplierError, &now, true
				state.Muze.CheckedAt = nil
				markRefillFailureReplacement(state, j)
				if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
					return err
				}
				return persist()
			}
			if err != nil || order == nil || !validMuzeIdentifier(order.ID, false) {
				j.Status, j.Error = "blocked", "muze 下单结果待确认，正在按原请求编号核对"
				if j.MuzeStudio {
					j.Error = muzeStudioReview
				}
				j.NextAttemptAt = nextRefillOrderPoll(err)
				var remote *refillProviderError
				if errors.As(err, &remote) {
					j.SupplierError = fmt.Sprintf("muze HTTP %d", remote.code)
					if remote.code == 429 {
						state.Muze.RetryAt = j.NextAttemptAt
					}
				}
				if rejection != nil {
					j.SupplierError = rejection.Error()
				}
				return persist()
			}
			if err := s.muzeOrderUnclaimed(ctx, state.Jobs, j.ID, order.ID); err != nil {
				j.Status, j.Error = "blocked", err.Error()
				j.NextAttemptAt = nextRefillOrderPoll(err)
				return persist()
			}
			j.SupplierID, j.Status, j.SupplierStatus = order.ID, "processing", "preparing"
			j.SupplierExpiresAt = order.ExpiresAt
			j.Error, j.NextAttemptAt = "", nextRefillOrderPoll(nil)
			state.Muze.CheckedAt = nil // Refresh prepaid quotas on the next worker tick.
			return persist()
		}
	}
	cycle, err := s.muzeClient().Cycle(ctx, token, j.SupplierID)
	j.Attempts++
	j.NextAttemptAt = nextRefillOrderPoll(err)
	if err != nil {
		j.Error = "muze订单查询暂不可用，继续核对原订单"
		var remote *refillProviderError
		if errors.As(err, &remote) && remote.code == 429 {
			state.Muze.RetryAt = j.NextAttemptAt
		}
		return persist()
	}
	if err := validateMuzeCycle(cycle, *j); err != nil {
		j.Status, j.Error = "blocked", err.Error()
		return persist()
	}
	if expires := cycle.expiresAt(); expires != nil {
		j.SupplierExpiresAt = expires
	}
	j.SupplierStatus = cycle.Status
	j.SupplierRefundPhase = cycle.RefundPhase
	if !cycle.Pushed && (cycle.Status == "failed" || cycle.RefundPhase == "returned" || cycle.Status == "returned") {
		j.Status, j.Error, j.FinishedAt = "failed", "muze 订单已结束，请以号商记录核对次数", &now
		j.BudgetReleased = cycle.RefundPhase == "returned" || cycle.Status == "returned"
		markRefillFailureReplacement(state, j)
		if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
			return err
		}
		return persist()
	}
	if !cycle.Pushed && cycle.ended() {
		finishRefillDeliveryExpired(j, now)
		return persist()
	}
	if !cycle.Pushed || cycle.Status == "purchaseReview" || cycle.Status == "failed" {
		j.Status, j.Error = "processing", "等待 muze 确认推送，继续核对原订单"
		return persist()
	}
	j.Status = "importing"
	if err := persist(); err != nil {
		return err
	}
	if err := s.importMuzeDelivery(ctx, j, cycle, state.Config.PushEnabled); err != nil {
		j.Status, j.Error = "import_failed", err.Error()
		if errors.Is(err, errMuzePushNotVisible) {
			j.Status = "processing"
		}
		if errors.Is(err, errRefillDeliveryExpired) {
			finishRefillDeliveryExpired(j, now)
			if j.AccountID > 0 {
				j.Error = "原订单已交付并结束，已补齐账号关联和回收时间，未重新启用"
			}
		}
		return persist()
	}
	if !state.Config.PushEnabled {
		j.Status, j.Error = "awaiting_push", "muze已推送，账号已按本单设置停用，等待恢复推送后启用"
		return persist()
	}
	j.Status, j.Error, j.FinishedAt = "imported", "", &now
	completeRefillExpiryDemand(state, *j)
	state.Message = "muze账号已推入号池，并已应用本单保存的推送设置"
	if err := s.notifyRefillImported(ctx, state, j, persist); err != nil {
		return err
	}
	return persist()
}

func (s *AccountRefillService) muzePushedAccount(ctx context.Context, j *AccountRefillJob, cycle *muzeCycle) (*Account, error) {
	// A supplier OAuth accountId is an upstream subject, never a Sub2 row ID.
	if cycle.AccountID != "" || j.AccountID > 0 {
		id := j.AccountID
		if cycle.AccountID != "" {
			n, err := strconv.ParseInt(cycle.AccountID, 10, 64)
			if err != nil || n <= 0 || (id > 0 && id != n) {
				return nil, errors.New("Muze 推送的本地账号编号不匹配")
			}
			id = n
		}
		rows, err := s.accounts.GetByIDs(ctx, []int64{id})
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || rows[0] == nil {
			return nil, errMuzePushNotVisible
		}
		if err := validateMuzePushedAccount(rows[0], j, cycle, time.Now()); err != nil {
			return nil, err
		}
		return rows[0], nil
	}
	if cycle.Subject == "" || strings.TrimSpace(cycle.Email) == "" {
		return nil, errors.New("Muze 交付缺少可核对的账号标识或邮箱")
	}
	lookup, ok := s.accounts.(interface {
		FindRefillPushCandidates(context.Context, []int64, string, time.Time) ([]*Account, error)
	})
	if !ok || j.SubmissionStartedAt == nil {
		return nil, errors.New("Muze 推送账号查找服务不可用")
	}
	rows, err := lookup.FindRefillPushCandidates(ctx, j.DeliveryGroupIDs, cycle.Subject, j.SubmissionStartedAt.Add(-30*time.Second))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errMuzePushNotVisible
	}
	if len(rows) != 1 {
		return nil, errors.New("Muze 交付匹配到多个本地账号，需要人工核对")
	}
	if err := validateMuzePushedAccount(rows[0], j, cycle, time.Now()); err != nil {
		return nil, err
	}
	return rows[0], nil
}

var errMuzePushNotVisible = errors.New("等待 Muze 推送账号出现在目标分组")

func validateMuzePushedAccount(a *Account, j *AccountRefillJob, cycle *muzeCycle, now time.Time) error {
	if a == nil || !cycle.Pushed || a.ID <= 0 || (j.AccountID > 0 && a.ID != j.AccountID) || !a.IsOpenAIOAuth() || a.IsShadow() {
		return errors.New("Muze 推送账号类型或编号不匹配")
	}
	if cycle.AccountID != "" && cycle.AccountID != strconv.FormatInt(a.ID, 10) {
		return errors.New("Muze 推送的本地账号编号不匹配")
	}
	email, _ := a.Credentials["email"].(string)
	subject, _ := a.Credentials["chatgpt_account_id"].(string)
	if strings.TrimSpace(cycle.Email) == "" || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(cycle.Email)) || (cycle.Subject != "" && cycle.Subject != subject) || (cycle.AccountID == "" && cycle.Subject == "") {
		return errors.New("Muze 推送账号的身份不匹配")
	}
	if cycle.PushedAt != "" {
		at, err := time.Parse(time.RFC3339Nano, cycle.PushedAt)
		if err != nil || a.CreatedAt.After(at.Add(time.Minute)) {
			return errors.New("账号创建时间晚于 Muze 本次推送")
		}
	}
	if marker, _ := a.Extra["refill_job_id"].(string); marker != "" && marker != j.ID {
		return errors.New("muze推送账号已属于其他补号任务")
	}
	if supplier, _ := a.Extra["refill_supplier_id"].(string); supplier != "" && supplier != cycle.ID {
		return errors.New("muze推送账号已属于其他号商订单")
	}
	if j.SubmissionStartedAt == nil || a.CreatedAt.IsZero() || a.CreatedAt.Before(j.SubmissionStartedAt.Add(-30*time.Second)) || a.CreatedAt.After(now.Add(30*time.Second)) {
		return errors.New("muze返回的账号创建时间与本次交付不匹配，未覆盖旧账号设置")
	}
	return nil
}

const muzeLeaseDuration = 100 * time.Minute

// Prefer the supplier's lease. Only a missing lease falls back to the original
// import time; polling, recovery and restarts never renew that fallback.
func muzeSourceExpiry(cycle *muzeCycle, a *Account) *int64 {
	if cycle != nil {
		if expires := cycle.expiresAt(); expires != nil {
			return expires
		}
	}
	if a == nil || a.CreatedAt.IsZero() {
		return nil
	}
	if a.Extra[AccountSourceExpiryOriginExtraKey] == "supplier_api" && a.Extra["import_source"] == "muze_api" {
		if saved := expiryTimestamp(a.Extra[AccountSourceExpiryExtraKey]); saved > 0 {
			return &saved
		}
	}
	return nil
}

// The old 100-minute local scheduling policy is only a local fallback. Never
// write it into the supplier deadline shown in the admin panel.
func muzeEffectiveLease(j *AccountRefillJob, c *muzeCycle, a *Account) int64 {
	if at := c.expiresAt(); at != nil {
		j.SupplierExpiresAt = at
	}
	if j.SupplierExpiresAt == nil {
		j.SupplierExpiresAt = muzeSourceExpiry(c, a)
	}
	if j.SupplierExpiresAt != nil {
		return *j.SupplierExpiresAt
	}
	return a.CreatedAt.Add(muzeLeaseDuration).Unix()
}
func muzeLeaseUpdates(j *AccountRefillJob, a *Account, expires int64, now time.Time) map[string]any {
	updates := map[string]any{AccountSourceExpiryExtraKey: expires, AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryCheckedExtraKey: now.UTC().Format(time.RFC3339)}
	if j.SupplierExpiresAt == nil {
		updates[AccountSourceExpiryExtraKey] = nil
		updates[AccountSourceExpiryOriginExtraKey] = "supplier_pending"
		updates[AccountSourceExpiryCheckedExtraKey] = nil
	}
	if a.HasNativeExpiry() {
		base := expires
		if j.DeliveryConfig.ExpiryMode == "fixed" && j.DeliveryConfig.FixedExpiresAt != nil {
			base = refillMin(base, *j.DeliveryConfig.FixedExpiresAt)
		}
		// Follow the supplier lease while respecting the saved fixed cutoff and
		// the account's current manual deadline / retention policy.
		updates[AccountExpiryBaseExtraKey] = base
	}
	return updates
}

func (s *AccountRefillService) importMuzeDelivery(ctx context.Context, j *AccountRefillJob, cycle *muzeCycle, activate bool) error {
	a, err := s.muzePushedAccount(ctx, j, cycle)
	if err != nil {
		return err
	}
	id := a.ID
	lease := muzeEffectiveLease(j, cycle, a)
	j.ExpiresAt = &lease
	ended := cycle.ended() || refillDeliveryExpired(j.ExpiresAt, time.Now()) ||
		(j.DeliveryConfig.ExpiryMode == "fixed" && refillDeliveryExpired(j.DeliveryConfig.FixedExpiresAt, time.Now()))
	if marker, _ := a.Extra["refill_job_id"].(string); marker == j.ID && !ended {
		if writer, ok := s.accounts.(interface {
			UpdateExtra(context.Context, int64, map[string]any) error
		}); ok {
			if err := writer.UpdateExtra(ctx, id, muzeLeaseUpdates(j, a, *j.ExpiresAt, time.Now())); err != nil {
				return err
			}
			a, err = s.muzePushedAccount(ctx, j, cycle)
			if err != nil {
				return err
			}
		}
		if !activate {
			if err := s.accounts.SetSchedulable(ctx, id, false); err != nil {
				return err
			}
			_, err := s.admin.UpdateAccount(ctx, id, &UpdateAccountInput{Status: "inactive"})
			if err == nil {
				j.AccountID = id
			}
			return err
		}
		if a.ExpiresAt != nil && !a.ExpiresAt.After(time.Now()) {
			j.AccountID = id
			return errRefillDeliveryExpired
		}
		return s.finishRefillImport(ctx, id, j)
	}
	// Build the same input as downloaded delivery, without copying an account
	// or persisting supplier OAuth credentials in the refill state.
	raw, err := json.Marshal(map[string]any{"accounts": []any{map[string]any{
		"name": a.Name, "platform": a.Platform, "type": a.Type, "credentials": a.Credentials, "expires_at": j.ExpiresAt,
	}}})
	if err != nil {
		return errors.New("无法读取muze推送账号")
	}
	var delivery RefillDelivery
	if json.Unmarshal(raw, &delivery) != nil {
		return errors.New("muze推送账号格式无效")
	}
	input, err := s.buildRefillDeliveryInput(ctx, j, &delivery, ended)
	if err != nil {
		return err
	}
	if j.SupplierExpiresAt == nil {
		input.Extra[AccountSourceExpiryExtraKey] = nil
		input.Extra[AccountSourceExpiryOriginExtraKey] = "supplier_pending"
		input.Extra[AccountSourceExpiryCheckedExtraKey] = nil
	}
	if ended {
		recorder, ok := s.admin.(interface {
			RecordExpiredRefillAccount(context.Context, int64, *CreateAccountInput) error
		})
		if !ok {
			return errors.New("muze历史交付核对服务不可用")
		}
		if err := recorder.RecordExpiredRefillAccount(ctx, id, input); err != nil {
			return errors.New("muze历史交付记录失败，请重试原任务")
		}
		j.AccountID = id
		return errRefillDeliveryExpired
	}
	adopter, ok := s.admin.(interface {
		AdoptRefillAccount(context.Context, int64, *CreateAccountInput) (*Account, error)
	})
	if !ok {
		return errors.New("muze推送设置应用服务不可用")
	}
	input.ProxyMode = j.DeliveryConfig.ProxyMode
	if _, err := adopter.AdoptRefillAccount(ctx, id, input); err != nil {
		return errors.New("muze账号推送设置应用失败，请核对分组和可用代理后重试原任务")
	}
	if !activate {
		j.AccountID = id
		return nil
	}
	return s.finishRefillImport(ctx, id, j)
}
