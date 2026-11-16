package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func validateFenduduCycle(c *fenduduCycle, j AccountRefillJob) error {
	if c == nil || !validFenduduIdentifier(c.ID, false) || (j.SupplierID != "" && c.ID != j.SupplierID) || c.Product != j.Plan ||
		j.SubmissionStartedAt == nil {
		return errors.New("粉嘟嘟订单的套餐、连接或编号与本任务不匹配")
	}
	// Pending orders may not have delivery metadata yet. Reject contradictory
	// metadata immediately, and require a complete match before adopting a push.
	if (c.PushProvider != "" || c.Pushed) && c.PushProvider != "sub2api" ||
		j.FenduduConnectionID != "" && (c.PushConfigID != "" || c.Pushed) && c.PushConfigID != j.FenduduConnectionID {
		return errors.New("粉嘟嘟订单的推送连接与本任务不匹配")
	}
	created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
	if err != nil || created.Before(j.SubmissionStartedAt.Add(-30*time.Second)) || created.After(j.SubmissionStartedAt.Add(5*time.Minute)) {
		return errors.New("粉嘟嘟订单创建时间与本次下单不匹配")
	}
	return nil
}

func (s *AccountRefillService) advanceFenduduJobs(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	index := -1
	for i, j := range state.Jobs {
		recoverable := j.SupplierID == "" && j.SubmissionStartedAt != nil
		if j.Provider != refillProviderFendudu || !j.pending() || now.Before(j.NextAttemptAt) || (j.Status == "blocked" && !recoverable) || j.Status == "import_failed" || (!state.Config.PushEnabled && j.Status == "awaiting_push") {
			continue
		}
		if index < 0 || j.NextAttemptAt.Before(state.Jobs[index].NextAttemptAt) {
			index = i
		}
	}
	if index < 0 {
		return nil
	}
	j := &state.Jobs[index]
	token, err := s.encryptor.Decrypt(state.Fendudu.EncryptedToken)
	if err != nil {
		j.Status, j.Error = "blocked", "粉嘟嘟对接码解密失败，请重新保存"
		return persist()
	}
	if err := s.notifyRefillStarted(ctx, state, j, persist); err != nil {
		return err
	}
	j.UpdatedAt = now
	if j.SupplierID == "" {
		if j.SubmissionStartedAt != nil {
			cycle, err := s.recoverFenduduOrder(ctx, token, state, *j)
			j.Attempts++
			if err != nil {
				j.Status, j.Error = "blocked", "粉嘟嘟尚未确认唯一交付，正在只读核对原订单；不会重复下单"
				j.NextAttemptAt = nextRefillOrderPoll(err)
				if j.SupplierStatus == "pending" {
					j.Status, j.Error = "processing", "粉嘟嘟已受理，等待原订单生成；正在只读核对，不会重复下单"
				}
				if errors.Is(err, errFenduduPushPending) {
					j.Error = errFenduduPushPending.Error()
					j.NextAttemptAt = nextRefillOrderPoll(nil)
					if expires := cycle.expiresAt(); expires != nil {
						j.SupplierExpiresAt = expires
					}
				}
				var remote *refillProviderError
				if errors.As(err, &remote) && remote.code == 429 {
					state.Fendudu.RetryAt = j.NextAttemptAt
				}
				return persist()
			}
			j.SupplierID, j.Status, j.Error, j.SupplierError = cycle.ID, "processing", "", ""
			state.Fendudu.CheckedAt = nil
			if err := persist(); err != nil {
				return err
			}
		} else {
			j.SubmissionStartedAt, j.Status = &now, "submitting"
			j.Attempts++
			if err := persist(); err != nil {
				return err
			}
			order, err := s.fenduduClient().Order(ctx, token, *j)
			var rejection *fenduduHTTPError
			definiteResponse := err == nil || (errors.As(err, &rejection) && slices.Contains([]int{
				http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired,
				http.StatusForbidden, http.StatusUnprocessableEntity,
			}, rejection.remote.code))
			if definiteResponse && order.rejected() {
				j.Status, j.SupplierStatus, j.SupplierError = "failed", "rejected", order.Error
				j.Error, j.FinishedAt, j.BudgetReleased = "粉嘟嘟拒绝下单："+order.Error, &now, true
				state.Fendudu.CheckedAt = nil
				if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
					return err
				}
				return persist()
			}
			if err == nil && order != nil && order.Success && order.Pending && order.Created == 0 && len(order.CycleIDs) == 0 {
				j.Status, j.SupplierStatus = "processing", "pending"
				j.Error = "粉嘟嘟已受理，等待原订单生成；正在只读核对，不会重复下单"
				j.SupplierError = strings.TrimSpace(strings.Join([]string{order.Error, order.Warning}, " "))
				j.NextAttemptAt = nextRefillOrderPoll(nil)
				state.Fendudu.CheckedAt = nil
				return persist()
			}
			if err != nil || order == nil || !order.Success || order.Created < 0 || order.Created > 1 || len(order.CycleIDs) != 1 || !validFenduduIdentifier(order.CycleIDs[0], false) {
				// Unknown results retain the original submission marker, including
				// legacy jobs submitted before supplier idempotency was supported.
				j.Status, j.Error = "blocked", "粉嘟嘟下单回执不完整，正在只读核对原订单；不会重复下单"
				j.SupplierError = "下单响应未包含唯一成功订单"
				if order != nil && (order.Error != "" || order.Warning != "") {
					j.SupplierError = strings.TrimSpace(strings.Join([]string{order.Error, order.Warning}, " "))
				}
				j.NextAttemptAt = nextRefillOrderPoll(err)
				if err != nil {
					j.SupplierError = "下单连接或响应读取失败"
				}
				var remote *refillProviderError
				if errors.As(err, &remote) {
					j.SupplierError = fmt.Sprintf("下单 HTTP %d，结果待核对", remote.code)
					if remote.code == 429 {
						state.Fendudu.RetryAt = now.Add(refillRetryDelay(err, 1))
					}
				}
				var responseError *fenduduHTTPError
				if errors.As(err, &responseError) {
					j.SupplierError = responseError.Error()
				}
				return persist()
			}
			if err := s.fenduduOrderUnclaimed(ctx, state.Jobs, j.ID, order.CycleIDs[0]); err != nil {
				j.Status, j.Error = "blocked", "粉嘟嘟返回的订单已关联其他任务，请核对"
				return persist()
			}
			j.SupplierID, j.Status, j.SupplierStatus = order.CycleIDs[0], "processing", "queued"
			j.SupplierError = strings.TrimSpace(strings.Join([]string{order.Error, order.Warning}, " "))
			j.Error, j.NextAttemptAt = "", nextRefillOrderPoll(nil)
			state.Fendudu.CheckedAt = nil // Refresh prepaid quotas on the next worker tick.
			return persist()
		}
	}
	cycle, err := s.fenduduClient().Cycle(ctx, token, j.SupplierID)
	j.Attempts++
	j.NextAttemptAt = nextRefillOrderPoll(err)
	if err != nil {
		j.Error = "粉嘟嘟订单查询暂不可用，继续核对原订单"
		var remote *refillProviderError
		if errors.As(err, &remote) && remote.code == 429 {
			state.Fendudu.RetryAt = j.NextAttemptAt
		}
		return persist()
	}
	if err := validateFenduduCycle(cycle, *j); err != nil {
		j.Status, j.Error = "blocked", err.Error()
		return persist()
	}
	if expires := cycle.expiresAt(); expires != nil {
		j.SupplierExpiresAt = expires
	}
	j.SupplierStatus = string([]rune(cycle.Status)[:refillMin(len([]rune(cycle.Status)), 128)])
	if cycle.Status == "order_stopped" {
		j.Status, j.Error, j.FinishedAt, j.BudgetReleased = "failed", "粉嘟嘟已停止订单，未扣预购次数", &now, true
		if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
			return err
		}
		return persist()
	}
	if !cycle.Pushed && (cycle.Status == "closed" || refillDeliveryExpired(cycle.expiresAt(), now)) {
		finishRefillDeliveryExpired(j, now)
		return persist()
	}
	if !cycle.Pushed || !slices.Contains([]string{"paid", "prewarming", "seated", "merging", "merged", "closed"}, cycle.Status) {
		j.Status, j.Error = "processing", "等待粉嘟嘟推送，原订单继续核对"
		return persist()
	}
	j.Status = "importing"
	if err := persist(); err != nil {
		return err
	}
	if err := s.importFenduduDelivery(ctx, j, cycle, state.Config.PushEnabled); err != nil {
		j.Status, j.Error = "import_failed", err.Error()
		if errors.Is(err, errRefillDeliveryExpired) {
			finishRefillDeliveryExpired(j, now)
			if j.AccountID > 0 {
				j.Error = "原订单已交付并结束，已补齐账号关联和回收时间，未重新启用"
			}
		}
		return persist()
	}
	if !state.Config.PushEnabled {
		j.Status, j.Error = "awaiting_push", "粉嘟嘟已推送，账号已按本单设置停用，等待恢复推送后启用"
		return persist()
	}
	j.Status, j.Error, j.FinishedAt = "imported", "", &now
	completeRefillExpiryDemand(state, *j)
	state.Message = "粉嘟嘟账号已推入号池，并已应用本单保存的推送设置"
	if err := s.notifyRefillImported(ctx, state, j, persist); err != nil {
		return err
	}
	return persist()
}

func (s *AccountRefillService) fenduduPushedAccount(ctx context.Context, j *AccountRefillJob, cycle *fenduduCycle) (*Account, error) {
	id, err := strconv.ParseInt(cycle.AccountID, 10, 64)
	if err != nil || id <= 0 || strings.TrimSpace(cycle.Email) == "" {
		return nil, errors.New("粉嘟嘟未返回有效的 SUB2 账号 ID 和邮箱，请核对推送连接")
	}
	accounts, err := s.accounts.GetByIDs(ctx, []int64{id})
	if err != nil || len(accounts) != 1 || accounts[0] == nil {
		return nil, errors.New("当前 SUB2 找不到粉嘟嘟推送的账号，请确认连接指向本实例后重试原任务")
	}
	a := accounts[0]
	if err := validateFenduduPushedAccount(a, j, cycle, time.Now()); err != nil {
		return nil, err
	}
	return a, nil
}

func validateFenduduPushedAccount(a *Account, j *AccountRefillJob, cycle *fenduduCycle, now time.Time) error {
	id, err := strconv.ParseInt(cycle.AccountID, 10, 64)
	if err != nil || id <= 0 || a == nil || strings.TrimSpace(cycle.Email) == "" {
		return errors.New("粉嘟嘟未返回有效的 SUB2 账号 ID 和邮箱")
	}
	email, _ := a.Credentials["email"].(string)
	if a.ID != id || !a.IsOpenAIOAuth() || a.IsShadow() || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(cycle.Email)) {
		return errors.New("粉嘟嘟推送账号的身份不匹配，未修改本地账号")
	}
	if marker, _ := a.Extra["refill_job_id"].(string); marker != "" && marker != j.ID {
		return errors.New("粉嘟嘟推送账号已属于其他补号任务")
	}
	if supplier, _ := a.Extra["refill_supplier_id"].(string); supplier != "" && supplier != cycle.ID {
		return errors.New("粉嘟嘟推送账号已属于其他号商订单")
	}
	if j.SubmissionStartedAt == nil || a.CreatedAt.IsZero() || a.CreatedAt.Before(j.SubmissionStartedAt.Add(-30*time.Second)) || a.CreatedAt.After(now.Add(30*time.Second)) {
		return errors.New("粉嘟嘟返回的账号创建时间与本次交付不匹配，未覆盖旧账号设置")
	}
	return nil
}

const fenduduLeaseDuration = 100 * time.Minute

// Prefer the supplier's lease. Only a missing lease falls back to the original
// import time; polling, recovery and restarts never renew that fallback.
func fenduduSourceExpiry(cycle *fenduduCycle, a *Account) *int64 {
	if cycle != nil {
		if expires := cycle.expiresAt(); expires != nil {
			return expires
		}
	}
	if a == nil || a.CreatedAt.IsZero() {
		return nil
	}
	if a.Extra["import_source"] == "fendudu_api" {
		if saved := expiryTimestamp(a.Extra[AccountSourceExpiryExtraKey]); saved > 0 {
			return &saved
		}
	}
	expires := a.CreatedAt.Add(fenduduLeaseDuration).Unix()
	return &expires
}

func fenduduLeaseUpdates(j *AccountRefillJob, a *Account, expires int64, now time.Time) map[string]any {
	updates := map[string]any{AccountSourceExpiryExtraKey: expires, AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryCheckedExtraKey: now.UTC().Format(time.RFC3339)}
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

func (s *AccountRefillService) importFenduduDelivery(ctx context.Context, j *AccountRefillJob, cycle *fenduduCycle, activate bool) error {
	a, err := s.fenduduPushedAccount(ctx, j, cycle)
	if err != nil {
		return err
	}
	id := a.ID
	j.ExpiresAt = fenduduSourceExpiry(cycle, a)
	ended := cycle.Status == "closed" || refillDeliveryExpired(j.ExpiresAt, time.Now()) ||
		(j.DeliveryConfig.ExpiryMode == "fixed" && refillDeliveryExpired(j.DeliveryConfig.FixedExpiresAt, time.Now()))
	if marker, _ := a.Extra["refill_job_id"].(string); marker == j.ID && !ended {
		if writer, ok := s.accounts.(interface {
			UpdateExtra(context.Context, int64, map[string]any) error
		}); ok {
			if err := writer.UpdateExtra(ctx, id, fenduduLeaseUpdates(j, a, *j.ExpiresAt, time.Now())); err != nil {
				return err
			}
			a, err = s.fenduduPushedAccount(ctx, j, cycle)
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
		return errors.New("无法读取粉嘟嘟推送账号")
	}
	var delivery RefillDelivery
	if json.Unmarshal(raw, &delivery) != nil {
		return errors.New("粉嘟嘟推送账号格式无效")
	}
	input, err := s.buildRefillDeliveryInput(ctx, j, &delivery, ended)
	if err != nil {
		return err
	}
	if ended {
		recorder, ok := s.admin.(interface {
			RecordExpiredRefillAccount(context.Context, int64, *CreateAccountInput) error
		})
		if !ok {
			return errors.New("粉嘟嘟历史交付核对服务不可用")
		}
		if err := recorder.RecordExpiredRefillAccount(ctx, id, input); err != nil {
			return errors.New("粉嘟嘟历史交付记录失败，请重试原任务")
		}
		j.AccountID = id
		return errRefillDeliveryExpired
	}
	adopter, ok := s.admin.(interface {
		AdoptRefillAccount(context.Context, int64, *CreateAccountInput) (*Account, error)
	})
	if !ok {
		return errors.New("粉嘟嘟推送设置应用服务不可用")
	}
	input.ProxyMode = j.DeliveryConfig.ProxyMode
	if _, err := adopter.AdoptRefillAccount(ctx, id, input); err != nil {
		return errors.New("粉嘟嘟账号推送设置应用失败，请核对分组和可用代理后重试原任务")
	}
	if !activate {
		j.AccountID = id
		return nil
	}
	return s.finishRefillImport(ctx, id, j)
}
