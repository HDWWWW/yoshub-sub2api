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
	RefillStarted       bool    `json:"refill_started"`
	LowBalance          bool    `json:"low_balance"`
	LowBalanceThreshold float64 `json:"low_balance_threshold"`
}
type AccountRefillNotificationStatus struct {
	At      *time.Time `json:"at"`
	Message string     `json:"message"`
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
	defer resp.Body.Close()
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
func (s *AccountRefillService) sendRefillNotification(ctx context.Context, state *accountRefillState, content string) {
	now := time.Now()
	state.NotificationStatus.At = &now
	webhook, err := s.encryptor.Decrypt(state.EncryptedWebhook)
	if err != nil {
		state.NotificationStatus.Message = "机器人地址解密失败，请重新保存"
		return
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
	}
}
func (s *AccountRefillService) notifyRefillStarted(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if !state.Config.Notifications.RefillStarted || state.EncryptedWebhook == "" || j.StartedNotificationAt != nil || j.Attempts > 0 || j.SupplierID != "" {
		return nil
	}
	now := time.Now()
	j.StartedNotificationAt = &now
	// WeCom has no idempotency key. Persist the attempt before sending so
	// retries and blue/green workers cannot repeatedly notify the same order.
	if err := persist(); err != nil {
		return err
	}
	mode := "自动补号"
	if j.ManualRequestID != "" {
		mode = "手动补号"
	}
	content := fmt.Sprintf("Sub2API 开始补号\n方式：%s\n套餐：%s\n积分：%g\n导入并发：%d\n投递分组 ID：%v\n任务：%s\n时间：%s", mode, j.Plan, j.Price, j.Concurrency, j.DeliveryGroupIDs, j.ID, now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	s.sendRefillNotification(ctx, state, content)
	return persist()
}
func (s *AccountRefillService) notifyRefillBalance(ctx context.Context, state *accountRefillState, balance, required float64, persist func() error) error {
	c := state.Config.Notifications
	if !c.LowBalance || state.EncryptedWebhook == "" || math.IsNaN(balance) || math.IsInf(balance, 0) {
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

// The existing refill lifecycle switch covers both starts and confirmed
// failures. Persist the attempt with the terminal result before contacting
// WeCom, whose API has no idempotency key.
func (s *AccountRefillService) notifyRefillFailed(ctx context.Context, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if !state.Config.Notifications.RefillStarted || state.EncryptedWebhook == "" ||
		j.FailedNotificationAt != nil || j.Status != "failed" || j.SupplierStatus != "failed" || j.SupplierID == "" {
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
		next = fmt.Sprintf("仍有缺口时按原套餐 %s 自动重补，不推进轮转、不叠加补号；继续受开关、冷却、在途、上限及预算约束。", j.Plan)
		if !state.Config.Enabled || !state.Config.MonitorEnabled || !state.Config.PushEnabled {
			next = "当前自动购买、监测或推送已暂停；恢复后重新检查缺口再决定是否重补原套餐。"
		}
	}
	content := fmt.Sprintf("Sub2API 补号失败\n方式：%s\n套餐：%s\n结果：号商已确认失败，本地预算预占已释放\n积分：%g\n投递分组 ID：%v\n任务：%s\n号商任务：%s\n后续：%s\n时间：%s", mode, j.Plan, j.Price, j.DeliveryGroupIDs, j.ID, j.SupplierID, next, now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	s.sendRefillNotification(ctx, state, content)
	return persist()
}

// Notification monitoring works independently of automatic purchases.
func (s *AccountRefillService) checkRefillNotificationBalance(ctx context.Context, token string, state *accountRefillState, now time.Time, persist func() error) error {
	if !state.Config.Notifications.LowBalance || state.EncryptedWebhook == "" || token == "" || now.Before(state.NextBalanceCheckAt) {
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
