package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type AccountRefillNotificationConfig struct {
	Channel             string  `json:"channel,omitempty"`
	QQAppID             string  `json:"qq_app_id,omitempty"`
	QQTargetType        string  `json:"qq_target_type,omitempty"`
	QQTargetOpenID      string  `json:"qq_target_openid,omitempty"`
	RefillStarted       bool    `json:"refill_started"`
	RefillNeeded        bool    `json:"refill_needed"`
	LowBalance          bool    `json:"low_balance"`
	LowBalanceThreshold float64 `json:"low_balance_threshold"`
}
type AccountRefillNotificationStatus struct {
	At      *time.Time `json:"at"`
	Message string     `json:"message"`
	NoRetry bool       `json:"no_retry,omitempty"`
}

func refillNotificationConfigured(state *accountRefillState) bool {
	if state.Config.Notifications.Channel == "qq" {
		return state.EncryptedQQSecret != "" && state.Config.Notifications.QQAppID != "" && state.Config.Notifications.QQTargetOpenID != ""
	}
	return state.EncryptedWebhook != ""
}

// This local monitor never reserves budget or advances purchase/rotation state.
// It runs before supplier backoff so unavailable supplier APIs cannot hide a
// pool shortage while automatic purchases are disabled.
func (s *AccountRefillService) checkDisabledRefillNotification(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	c := state.Config
	if c.Enabled || !c.MonitorEnabled || !c.Notifications.RefillNeeded || !refillNotificationConfigured(state) || now.Before(state.NextNeededCheckAt) {
		return nil
	}
	state.NextNeededCheckAt = now.Add(time.Duration(max(accountRefillMinIntervalSeconds, c.CheckSeconds)) * time.Second)
	if !refillInWindow(c, now) {
		return persist()
	}
	if err := c.Validate(); err != nil {
		state.NotificationStatus = AccountRefillNotificationStatus{At: &now, Message: "补号规则无效，未发送需求提醒"}
		return persist()
	}
	if err := s.evaluate(ctx, state, now); err != nil {
		state.NotificationStatus = AccountRefillNotificationStatus{At: &now, Message: "本地补号评估失败，未发送需求提醒"}
		return persist()
	}
	e := state.Evaluation
	s.invalidateCapacityObservations(c, e)
	if c.Mode == "interval" {
		if state.NextNeededIntervalAt.IsZero() {
			state.NextNeededIntervalAt = state.NextIntervalAt
			if state.NextNeededIntervalAt.IsZero() {
				state.NextNeededIntervalAt = now.Add(time.Duration(c.IntervalMinutes) * time.Minute)
			}
		}
		retryDue := state.RefillNeededAlerted && !state.RefillNeededRetryAt.IsZero() && !now.Before(state.RefillNeededRetryAt)
		if now.Before(state.NextNeededIntervalAt) && !retryDue {
			return persist()
		}
		e.Needed = min(c.IntervalQuantity, max(0, c.MaxReady-e.Ready-e.Pending), max(0, c.MaxInFlight-e.Pending))
		e.Reason = "定时补号"
		jobs, err := s.evaluationJobs(ctx, state)
		if err != nil {
			return persist()
		}
		for _, job := range jobs {
			if refillNeedsAttention(job.Status) && !refillJobTimedOut(c, job, now) {
				e.Needed = 0
			}
		}
	}
	if e.Needed <= 0 {
		state.RefillNeededAlerted = false
		state.RefillNeededRetryAt = time.Time{}
		return persist()
	}
	if refillNeedsCapacityObservation(e.Reason) {
		if !s.refillObservationReady(c, state.Revision, e, now) {
			return persist()
		}
	}
	immediateReplacement := nextRefillFailureReplacement(state) >= 0 && refillImmediateFailureRetryEnabled(c)
	if !immediateReplacement && now.Sub(state.LastSubmitAt) < time.Duration(c.CooldownSeconds)*time.Second {
		return persist()
	}
	if state.RefillNeededAlerted {
		if !state.RefillNeededRetryAt.IsZero() {
			if now.Before(state.RefillNeededRetryAt) {
				return persist()
			}
		} else if c.Mode != "interval" {
			return persist()
		}
	}
	if c.Mode == "interval" && state.RefillNeededRetryAt.IsZero() {
		state.NextNeededIntervalAt = now.Add(time.Duration(c.IntervalMinutes) * time.Minute)
	}
	state.RefillNeededAlerted = true
	state.RefillNeededRetryAt = now.Add(5 * time.Minute)
	if c.Notifications.Channel == "qq" {
		// Persist an at-most-once attempt before QQ I/O; only a confirmed
		// retryable failure below may arm a new delivery after interruption.
		state.RefillNeededRetryAt = time.Time{}
	}
	// WeCom keeps its bounded retry deadline; QQ only rearms a retry after
	// an explicit retryable result, never after an interrupted send.
	if err := persist(); err != nil {
		return err
	}
	groupLabel := fmt.Sprintf("#%d", c.GroupID)
	if group, err := s.groups.GetByID(ctx, c.GroupID); err == nil && group != nil && group.Name != "" {
		groupLabel = fmt.Sprintf("%s (#%d)", group.Name, c.GroupID)
	}
	content := fmt.Sprintf("Sub2API 补号需求提醒\n已触发自动补号规则，但自动补号已关闭，未自动补号。\n监测分组：%s\n触发原因：%s\n规则详情：%s\n当前待补：%d 个\n下次套餐：%s\n请按需手动补号或开启自动补号。\n时间：%s", groupLabel, e.Reason, accountRefillReasonDetail(c, e), e.Needed, nextRefillPurchasePlan(state), now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	if s.sendRefillNotification(ctx, state, content) || state.NotificationStatus.NoRetry {
		state.RefillNeededRetryAt = time.Time{}
		if c.Mode == "interval" {
			// A delayed successful retry starts a fresh notification interval.
			// Do not emit another success immediately for an elapsed deadline.
			state.NextNeededIntervalAt = now.Add(time.Duration(c.IntervalMinutes) * time.Minute)
		}
	} else {
		state.RefillNeededRetryAt = now.Add(5 * time.Minute)
		state.NotificationStatus.Message = strings.TrimSuffix(state.NotificationStatus.Message, "；补号继续处理") + "；自动补号已关闭，未下单；补号需求仍存在时至少 5 分钟后重试"
	}
	return persist()
}

type refillNotifier interface {
	Send(context.Context, string, string) error
}
type weComRefillNotifier struct{ client *http.Client }

func validateRefillWebhook(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "qyapi.weixin.qq.com" || u.Path != "/cgi-bin/webhook/send" || u.RawPath != "" || u.User != nil || u.Fragment != "" {
		return errors.New("请输入企业微信群机器人官方 Webhook 地址")
	}
	q, err := url.ParseQuery(u.RawQuery)
	key := q.Get("key")
	if err != nil || len(q) != 1 || len(q["key"]) != 1 || len(key) < 1 || len(key) > 256 || strings.ContainsAny(key, "\r\n\t ") {
		return errors.New("企业微信机器人 Webhook 缺少有效 key")
	}
	return nil
}
func (n weComRefillNotifier) Send(ctx context.Context, webhook, content string) error {
	if err := validateRefillWebhook(webhook); err != nil {
		return err
	}
	// Same robot text payload as NewAPI; HTTP success alone is not sufficient.
	body, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": content}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return errors.New("通知请求构建失败")
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("企业微信连接失败或超时")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("企业微信通知请求未成功")
	}
	var result struct {
		ErrCode *int `json:"errcode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil || result.ErrCode == nil || *result.ErrCode != 0 {
		return errors.New("企业微信机器人拒绝通知，请检查 Webhook 和群机器人状态")
	}
	return nil
}
func (s *AccountRefillService) sendRefillNotification(ctx context.Context, state *accountRefillState, content string) bool {
	now := time.Now()
	state.NotificationStatus = AccountRefillNotificationStatus{At: &now}
	if state.Config.Notifications.Channel == "qq" {
		secret, err := s.encryptor.Decrypt(state.EncryptedQQSecret)
		if err != nil {
			state.NotificationStatus.Message = "QQ 密钥解密失败，请重新保存"
			state.NotificationStatus.NoRetry = true
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		root := s.rootService()
		root.rulesMu.Lock()
		if root.qqNotifier == nil {
			root.qqNotifier = &qqRefillNotifier{}
		}
		notifier := root.qqNotifier
		root.rulesMu.Unlock()
		if err := notifier.Send(ctx, state.Config.Notifications, secret, content); err != nil {
			state.NotificationStatus.Message = err.Error() + "；补号继续处理"
			var deliveryErr *qqRefillDeliveryError
			state.NotificationStatus.NoRetry = errors.As(err, &deliveryErr) && !deliveryErr.retryable
			return false
		}
		state.NotificationStatus.Message = "QQ 已确认接收通知"
		return true
	}
	webhook, err := s.encryptor.Decrypt(state.EncryptedWebhook)
	if err != nil {
		state.NotificationStatus.Message = "机器人地址解密失败，请重新保存"
		return false
	}
	notifier := s.notifier
	if notifier == nil {
		notifier = weComRefillNotifier{}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := notifier.Send(ctx, webhook, content); err != nil {
		state.NotificationStatus.Message = "通知发送失败或结果未确认，请检查机器人配置；补号继续处理"
	} else {
		state.NotificationStatus.Message = "企业微信通知发送成功"
		return true
	}
	return false
}
func (s *AccountRefillService) notifyRefillStarted(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if !state.Config.Notifications.RefillStarted || !refillNotificationConfigured(state) || j.StartedNotificationAt != nil || j.Attempts > 0 || j.SupplierID != "" {
		return nil
	}
	now := time.Now()
	j.StartedNotificationAt = &now
	// WeCom has no idempotency key. Persist the attempt before sending so
	// retries and blue/green workers cannot repeatedly notify the same order.
	if err := persist(); err != nil {
		return err
	}
	content := fmt.Sprintf("Sub2API 开始补号\n方式：%s\n套餐：%s\n%s\n导入并发：%d\n投递分组 ID：%v\n号商任务：%s\n时间：%s", refillNotificationMode(j), j.Plan, refillNotificationCost(*j), j.Concurrency, j.DeliveryGroupIDs, refillSupplierTaskID(*j), now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	s.sendRefillNotification(ctx, state, content)
	return persist()
}
func (s *AccountRefillService) notifyRefillBalance(ctx context.Context, state *accountRefillState, balance, required float64, persist func() error) error {
	c := state.Config.Notifications
	if !c.LowBalance || !refillNotificationConfigured(state) || math.IsNaN(balance) || math.IsInf(balance, 0) {
		return nil
	}
	state.LowBalanceRequired = math.Max(state.LowBalanceRequired, required)
	if balance > c.LowBalanceThreshold && balance >= state.LowBalanceRequired {
		state.LowBalanceAlerted = false
		state.LowBalanceRequired = 0
		return persist()
	}
	if state.LowBalanceAlerted || (balance > c.LowBalanceThreshold && balance >= required) {
		return nil
	}
	state.LowBalanceAlerted = true
	if err := persist(); err != nil {
		return err
	}
	content := fmt.Sprintf("Sub2API 号商积分预警\n可用积分：%g\n预警阈值：%g\n当前补号所需积分：%g\n请及时充值，余额持续不足期间不重复提醒。\n时间：%s", balance, c.LowBalanceThreshold, required, time.Now().In(refillTimezone).Format("2006-01-02 15:04:05"))
	s.sendRefillNotification(ctx, state, content)
	return persist()
}

// The existing refill lifecycle switch covers starts, confirmed failures,
// completed imports and delivery delays for both manual and automatic orders. Persist attempts before contacting
// WeCom, whose API has no idempotency key.
func (s *AccountRefillService) notifyRefillFailed(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	fenduduRejected := j.Provider == refillProviderFendudu && j.SupplierStatus == "rejected"
	if !state.Config.Notifications.RefillStarted || !refillNotificationConfigured(state) ||
		j.FailedNotificationAt != nil || j.Status != "failed" ||
		(!fenduduRejected && ((j.SupplierStatus != "failed" && (j.Provider != refillProviderFendudu || j.SupplierStatus != "order_stopped")) || j.SupplierID == "")) {
		return nil
	}
	now := time.Now()
	j.FailedNotificationAt = &now
	if err := persist(); err != nil {
		return err
	}
	mode, next := "自动补号", "未安排自动重补，请核对当前规则与任务。"
	if j.ManualRequestID != "" || j.Reason == "手动补号" {
		mode, next = "手动补号", "手动订单不自动重下，请核对后按需手动补号。"
	} else if j.ReplacementPending {
		next = fmt.Sprintf("仍有缺口时立即按原套餐 %s 自动重补 1 个，跳过普通下单冷却；不推进轮转、不叠加补号；继续受开关、时段、在途、上限、预算及号商限流约束。", j.Plan)
		if !refillImmediateFailureRetryEnabled(state.Config) {
			next = fmt.Sprintf("立即重补已关闭；仍有缺口时按原套餐 %s 自动重补 1 个，遵守普通检测间隔和下单冷却；不推进轮转、不叠加补号；继续受开关、时段、在途、上限、预算及号商限流约束。", j.Plan)
		}
		if !state.Config.Enabled || !state.Config.MonitorEnabled || !state.Config.PushEnabled {
			next = "当前自动购买、监测或推送已暂停；恢复后重新检查缺口再决定是否重补原套餐。"
		}
	}
	content := fmt.Sprintf("Sub2API 补号失败\n方式：%s\n套餐：%s\n结果：号商已确认失败，本地预算预占已释放\n积分：%g\n投递分组 ID：%v\n号商任务：%s\n后续：%s\n时间：%s", mode, j.Plan, j.Price, j.DeliveryGroupIDs, refillSupplierTaskID(*j), next, now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	if j.Provider == refillProviderFendudu {
		content = fmt.Sprintf("Sub2API 粉嘟嘟手动补号失败\n结果：号商已停止订单，未扣次数\n套餐：%s\n号商任务：%s\n后续：请按需手动补号。", j.Plan, refillSupplierTaskID(*j))
		if fenduduRejected {
			content = fmt.Sprintf("Sub2API 粉嘟嘟手动补号失败\n结果：号商拒绝本次下单，未创建订单\n套餐：%s\n原因：%s\n后续：请处理错误后按需手动补号。", j.Plan, j.SupplierError)
		}
	}
	s.sendRefillNotification(ctx, state, content)
	return persist()
}

func automaticRefillJob(j *AccountRefillJob) bool {
	return j.ManualRequestID == "" && j.Reason != "手动补号"
}

func refillNotificationMode(j *AccountRefillJob) string {
	mode := "自动补号"
	if !automaticRefillJob(j) {
		mode = "手动补号"
	}
	if j.Provider == refillProviderFendudu {
		mode = "粉嘟嘟" + mode
	}
	return mode
}

func (s *AccountRefillService) notifyRefillImported(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if !state.Config.Notifications.RefillStarted || !refillNotificationConfigured(state) ||
		j.Status != "imported" || j.AccountID == 0 || j.ImportedNotificationAt != nil {
		return nil
	}
	now := time.Now()
	j.ImportedNotificationAt = &now
	// Save the completed import and notification attempt together. A restart
	// must neither re-import the account nor announce this completion twice.
	if err := persist(); err != nil {
		return err
	}
	content := fmt.Sprintf("Sub2API %s成功\n结果：账号已导入并绑定分组\n套餐：%s\n%s\n账号 ID：%d\n投递分组 ID：%v\n号商任务：%s\n耗时：%s\n时间：%s",
		refillNotificationMode(j), j.Plan, refillNotificationCost(*j), j.AccountID, j.DeliveryGroupIDs, refillSupplierTaskID(*j), now.Sub(j.CreatedAt).Round(time.Second), now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	s.sendRefillNotification(ctx, state, content)
	return persist()
}

const refillDeliveryAlertAfter = 5 * time.Minute

func (s *AccountRefillService) notifyRefillDelayed(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) error {
	if !state.Config.Notifications.RefillStarted || !refillNotificationConfigured(state) {
		return nil
	}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if !j.pending() || j.AccountID != 0 || j.DelayedNotificationAt != nil ||
			j.CreatedAt.IsZero() || now.Sub(j.CreatedAt) < refillDeliveryAlertAfter {
			continue
		}
		j.DelayedNotificationAt = &now
		if err := persist(); err != nil {
			return err
		}
		content := fmt.Sprintf("Sub2API %s超时提醒\n结果：下单已满 5 分钟，账号仍未成功导入\n套餐：%s\n%s\n当前状态：%s\n投递分组 ID：%v\n号商任务：%s\n已等待：%s\n继续核对原任务；是否另补账号按本组超时重试规则判断。本条延迟提醒每个任务只发送一次。\n时间：%s",
			refillNotificationMode(j), j.Plan, refillNotificationCost(*j), j.Status, j.DeliveryGroupIDs, refillSupplierTaskID(*j), now.Sub(j.CreatedAt).Round(time.Second), now.In(refillTimezone).Format("2006-01-02 15:04:05"))
		s.sendRefillNotification(ctx, state, content)
		if err := persist(); err != nil {
			return err
		}
	}
	return nil
}

// Notification monitoring works independently of automatic purchases.
func (s *AccountRefillService) checkRefillNotificationBalance(ctx context.Context, token string, state *accountRefillState, now time.Time, persist func() error) error {
	if !state.Config.Notifications.LowBalance || !refillNotificationConfigured(state) || token == "" || now.Before(state.NextBalanceCheckAt) {
		return nil
	}
	state.NextBalanceCheckAt = now.Add(time.Minute)
	wallet, _, err := s.provider.Identity(ctx, token)
	if err == nil && wallet != nil {
		state.Wallet, state.IdentityAt = wallet, &now
		if err := s.notifyRefillBalance(ctx, state, wallet.Available, 0, persist); err != nil {
			return err
		}
	} else {
		state.NotificationStatus = AccountRefillNotificationStatus{At: &now, Message: "余额读取失败，稍后重试；未据此发送余额预警"}
		var remote *refillProviderError
		if errors.As(err, &remote) && remote.code == 429 {
			state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
		}
	}
	return persist()
}

func refillNotificationCost(j AccountRefillJob) string {
	if j.Provider == refillProviderFendudu {
		return "预购次数：1"
	}
	return fmt.Sprintf("积分：%g", j.Price)
}
