package service

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
	"unicode"
)

type AccountRiskView struct {
	*AccountRiskRule
	TestAccounts         []AccountRiskTestAccount `json:"test_accounts"`
	WebhookStatus        string                   `json:"webhook_status"`
	NotificationChannels map[string]string        `json:"notification_channels"`
}

type accountRiskNotifier interface {
	RiskNotificationChannels(context.Context, int64) (map[string]bool, error)
	SendRiskNotification(context.Context, int64, string, string) error
}

// Reuse the selected refill rule's connection, including its existing inheritance
// policy. Reading it neither enables purchases nor changes refill notification state.
func (s *AccountRefillService) riskNotificationConnection(ctx context.Context, groupID int64) (*AccountRefillService, *accountRefillState, error) {
	rule, err := s.ForGroup(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	raw, err := rule.store.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	state, err := decodeAccountRefillState(raw)
	return rule, &state, err
}

func (s *AccountRefillService) RiskNotificationChannels(ctx context.Context, groupID int64) (map[string]bool, error) {
	_, state, err := s.riskNotificationConnection(ctx, groupID)
	if err != nil {
		return nil, err
	}
	qq := state.Config.Notifications
	qq.Channel = "qq"
	return map[string]bool{"wecom": state.EncryptedWebhook != "", "qq": state.EncryptedQQSecret != "" && validateRefillQQConfig(qq) == nil}, nil
}

func (s *AccountRefillService) SendRiskNotification(ctx context.Context, groupID int64, channel, content string) error {
	if channel != "wecom" && channel != "qq" {
		return errors.New("风控通知渠道无效")
	}
	rule, state, err := s.riskNotificationConnection(ctx, groupID)
	if err != nil {
		return errors.New("风控通知配置无法读取")
	}
	if channel == "qq" {
		config := state.Config.Notifications
		config.Channel = "qq"
		if state.EncryptedQQSecret == "" || validateRefillQQConfig(config) != nil {
			return errors.New("QQ 机器人未配置，请先保存此分组的 QQ 通知接口")
		}
		secret, err := rule.encryptor.Decrypt(state.EncryptedQQSecret)
		if err != nil {
			return errors.New("QQ 机器人密钥无法解密")
		}
		return rule.qqNotificationSender().Send(ctx, config, secret, content)
	}
	if state.EncryptedWebhook == "" {
		return errors.New("企业微信机器人未配置或无法读取")
	}
	webhook, err := rule.encryptor.Decrypt(state.EncryptedWebhook)
	if err != nil {
		return errors.New("企业微信机器人配置无法解密")
	}
	notifier := rule.notifier
	if notifier == nil {
		notifier = weComRefillNotifier{}
	}
	return notifier.Send(ctx, webhook, content)
}

func riskNotificationName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
	runes := []rune(name)
	if len(runes) > 100 {
		runes = runes[:100]
	}
	return string(runes)
}

func riskAccountEmail(account *Account) string {
	if account == nil {
		return ""
	}
	email, _ := account.Credentials["email"].(string)
	return riskNotificationEmail(email, account.Name)
}

// Capture only a validated email, never a credential blob or provider response.
func riskNotificationEmail(email, name string) string {
	for _, candidate := range []string{email, name} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || len(candidate) > 254 || strings.ContainsFunc(candidate, unicode.IsControl) {
			continue
		}
		address, err := mail.ParseAddress(candidate)
		if err == nil && address.Address == candidate {
			return candidate
		}
	}
	return ""
}

// Older queued probe events carry the cooldown deadline in their reason text.
func riskNotificationReason(e AccountRiskEvent) string {
	const marker = "，凭据刷新冷却至 "
	if e.Kind == accountRiskProbe401 && e.Action == "cooldown" {
		if prefix, stamp, found := strings.Cut(e.Reason, marker); found {
			if deadline, err := time.Parse(time.RFC3339, stamp); err == nil {
				return prefix + marker + formatNotificationTime(deadline)
			}
		}
	}
	return e.Reason
}

func (s *AccountRiskService) wakeNotifications() {
	select {
	case s.notificationWake <- struct{}{}:
	default:
	}
}

func (s *AccountRiskService) notificationLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.deliverNotification(ctx, time.Now().UTC())
		case <-s.notificationWake:
			_ = s.deliverNotification(ctx, time.Now().UTC())
		}
	}
}

// A stop and its pending notification commit in the same transaction. A separate
// worker durably claims each event and its channel before any network I/O.
// Uncertain sends are never replayed, including after a restart or slot switch.
func (s *AccountRiskService) deliverNotification(ctx context.Context, now time.Time) error {
	var event AccountRiskEvent
	var groupID int64
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := s.store.Update(claimCtx, func(state *AccountRiskState, _ AccountRiskScheduling) error {
		ids := make([]int64, 0, len(state.Rules))
		inFlight := false
		for id, r := range state.Rules {
			ids = append(ids, id)
			for i := range r.Events {
				e := &r.Events[i]
				if e.Notification == "sending" {
					if e.NotificationAt != nil && now.Sub(*e.NotificationAt) < 15*time.Second {
						inFlight = true
					} else {
						e.Notification = "unknown"
					}
				}
				if e.Notification == "pending" && !riskEventNotificationEnabled(r.Config, *e) {
					e.Notification = "cancelled"
				}
			}
			trimAccountRiskEvents(r)
		}
		if inFlight || now.Before(state.NextNotificationAt) {
			return nil
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var selected *AccountRiskEvent
		for _, id := range ids {
			for i := range state.Rules[id].Events {
				e := &state.Rules[id].Events[i]
				if e.Notification == "pending" && (selected == nil || e.At.Before(selected.At)) {
					selected, groupID = e, id
				}
			}
		}
		if selected != nil {
			selected.Notification, selected.NotificationAt = "sending", &now
			selected.NotificationChannel = riskNotificationChannel(state.Rules[groupID].Config)
			state.NextNotificationAt = now.Add(3 * time.Second)
			event = *selected
		}
		return nil
	})
	cancel()
	if err != nil || event.ID == "" {
		return err
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
	groupName := ""
	if g, err := s.groups.GetByID(sendCtx, groupID); err == nil && g != nil {
		groupName = riskNotificationName(g.Name)
	}
	action := "已关闭此账号调度，现有请求继续完成。"
	switch event.Action {
	case riskActionReduceConcurrency:
		action = "已降低此账号并发上限，继续调度。现有请求继续完成。"
	case "observed":
		action = "已记录本次上游 401。"
	case "gateway":
		action = "已记录业务请求的上游 401，调度状态由网关现有认证规则处理。"
	case "cooldown":
		action = "已暂停接收新请求，等待凭据刷新。冷却截止时间见上方原因。"
	case "notify_only":
		action = "仅通知，未因本次异常停止账号调度。"
	case "pause":
		action = "已暂停此账号调度，消息复测通过后自动恢复，现有请求继续完成。"
	}
	email := riskNotificationEmail(event.AccountEmail, event.AccountName)
	if email == "" {
		email = "未提供"
	}
	content := fmt.Sprintf("SUB2API 风控检测告警\n分组：#%d %s\n账号 ID：#%d\n账号名称：%s\n账号邮箱：%s\n北京时间：%s\n原因：%s\n处理：%s", groupID, groupName, event.AccountID, event.AccountName, email, formatNotificationTime(event.At), riskNotificationReason(event), action)
	if event.Occurrences > 1 {
		content += fmt.Sprintf("\n合并异常次数：%d", event.Occurrences)
	}
	status := "failed"
	if s.notifier != nil && s.notifier.SendRiskNotification(sendCtx, groupID, event.NotificationChannel, content) == nil {
		status = "sent"
	}
	sendCancel()
	// Only retry saving the outcome, never the notification itself.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer finishCancel()
	for attempt := 0; attempt < 5; attempt++ {
		err = s.store.Update(finishCtx, func(state *AccountRiskState, _ AccountRiskScheduling) error {
			if r := state.Rules[groupID]; r != nil {
				for i := range r.Events {
					if r.Events[i].ID == event.ID {
						r.Events[i].Notification = status
						break
					}
				}
				trimAccountRiskEvents(r)
			}
			return nil
		})
		if !errors.Is(err, ErrAccountRiskBusy) {
			return err
		}
		select {
		case <-finishCtx.Done():
			return finishCtx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return err
}

// A simultaneous batch can exceed the visible history limit. Retain every
// unsent/claimed notification until its outcome is saved; the UI shows 50 rows.
func trimAccountRiskEvents(r *AccountRiskRule) {
	if len(r.Events) <= 50 {
		return
	}
	kept := r.Events[:50]
	for _, event := range r.Events[50:] {
		if event.Notification == "pending" || event.Notification == "sending" {
			kept = append(kept, event)
		}
	}
	r.Events = kept
}
