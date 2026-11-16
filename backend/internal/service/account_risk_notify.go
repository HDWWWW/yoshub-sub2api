package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

type AccountRiskView struct {
	*AccountRiskRule
	TestAccounts  []AccountRiskTestAccount `json:"test_accounts"`
	WebhookStatus string                   `json:"webhook_status"`
}

type accountRiskNotifier interface {
	RiskWebhookConfigured(context.Context, int64) (bool, error)
	SendRiskNotification(context.Context, int64, string) error
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

func (s *AccountRefillService) RiskWebhookConfigured(ctx context.Context, groupID int64) (bool, error) {
	_, state, err := s.riskNotificationConnection(ctx, groupID)
	if err != nil {
		return false, err
	}
	return state.EncryptedWebhook != "", nil
}

func (s *AccountRefillService) SendRiskNotification(ctx context.Context, groupID int64, content string) error {
	rule, state, err := s.riskNotificationConnection(ctx, groupID)
	if err != nil || state.EncryptedWebhook == "" {
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
// worker durably claims each event before any network I/O. WeCom has no idempotency
// key: uncertain sends are never replayed, including after a restart or slot switch.
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
	case "notify_only":
		action = "仅通知，未因本次异常停止账号调度。"
	case "pause":
		action = "已暂停此账号调度，消息复测通过后自动恢复，现有请求继续完成。"
	}
	content := fmt.Sprintf("SUB2API 风控检测告警\n分组：#%d %s\n账号：#%d %s\n时间：%s\n原因：%s\n处理：%s", groupID, groupName, event.AccountID, event.AccountName, event.At.Format(time.RFC3339), event.Reason, action)
	if event.Occurrences > 1 {
		content += fmt.Sprintf("\n合并异常次数：%d", event.Occurrences)
	}
	status := "failed"
	if s.notifier != nil && s.notifier.SendRiskNotification(sendCtx, groupID, content) == nil {
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
