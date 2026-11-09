package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const accountRiskModelObserverKey = "account_risk_model_observer"

var errAccountRiskModelAccountChanged = errors.New("account changed before model mismatch stop")

func riskModelMismatchEnabled(c AccountRiskConfig) bool {
	return c.Enabled && (c.ModelMismatchAction == "stop" || c.ModelMismatchAction == "notify_only")
}

func riskEventNotificationEnabled(c AccountRiskConfig, event AccountRiskEvent) bool {
	if event.Kind == "model_mismatch" {
		return riskModelMismatchEnabled(c)
	}
	if event.Action == "notify_only" {
		switch event.Kind {
		case accountRiskProbeCapacity:
			return c.Enabled && c.CapacityNotifyOnly
		case accountRiskProbeSingle:
			return c.Enabled && c.ProbeNotifyOnly
		}
	}
	return c.Enabled && c.NotifyEnabled
}

func riskModelAccountIdentity(a *Account) [32]byte {
	data, _ := json.Marshal([]any{a.Platform, a.Type, a.Credentials, a.ParentAccountID})
	return sha256.Sum256(data)
}

// HandleModelMismatch acts on a declaration from the actual upstream, before any
// downstream model-name rewrite. It never waits for the periodic risk probe or
// for asynchronous billing. Missing declarations are not evidence of a mismatch.
func (s *AccountRiskService) HandleModelMismatch(ctx context.Context, groupID int64, account *Account, sentModel, responseModel string, now time.Time) error {
	sentModel = normalizeObservedUpstreamResponseModel(sentModel)
	responseModel = normalizeObservedUpstreamResponseModel(responseModel)
	if s == nil || s.store == nil || account == nil || account.Platform != PlatformOpenAI || groupID <= 0 || sentModel == "" || responseModel == "" || upstreamModelsMatchForAudit(sentModel, responseModel) {
		return nil
	}
	recorded := false
	err := s.store.Update(ctx, func(state *AccountRiskState, stop AccountRiskScheduling) error {
		r := state.Rules[groupID]
		if r == nil || !riskModelMismatchEnabled(r.Config) {
			return nil
		}
		g, err := s.groups.GetByID(ctx, groupID)
		if err != nil {
			return err
		}
		if g == nil || g.Platform != PlatformOpenAI || g.Status != StatusActive {
			return nil
		}
		current, err := s.accounts.GetByID(ctx, account.ID)
		if err != nil {
			return err
		}
		if current == nil || current.Status != StatusActive || (!current.Schedulable && !AccountHasRiskCapacityPause(current)) || !slices.Contains(current.GroupIDs, groupID) || riskModelAccountIdentity(current) != riskModelAccountIdentity(account) {
			return nil
		}
		action := r.Config.ModelMismatchAction
		if action == "stop" {
			changed, err := stop.Stop(groupID, current)
			if err != nil {
				return err
			}
			if !changed {
				return errAccountRiskModelAccountChanged
			}
		} else {
			// First discovery alerts immediately. Coalesce the same account for
			// one minute so concurrent streams cannot exhaust the robot quota.
			// The first timestamp never moves, so ongoing anomalies alert again.
			for i := range r.Events {
				e := &r.Events[i]
				if e.Kind == "model_mismatch" && e.Action == "notify_only" && e.Notification != "cancelled" && e.AccountID == current.ID && !now.Before(e.At) && now.Sub(e.At) < time.Minute {
					e.Occurrences++
					return nil
				}
			}
		}
		reason := fmt.Sprintf("上游响应模型不一致：实际发送 %q，实际返回 %q", riskNotificationName(sentModel), riskNotificationName(responseModel))
		if action == "stop" {
			reason += "，已关闭账号调度"
			riskReset(&r.Observation, "stopped", reason)
		} else {
			reason += "，仅通知，未停止调度"
		}
		event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: current.ID, AccountName: riskNotificationName(current.Name), Reason: reason, Kind: "model_mismatch", Action: action, Occurrences: 1, Notification: "pending"}
		r.Events = append([]AccountRiskEvent{event}, r.Events...)
		if len(r.Events) > 50 {
			r.Events = r.Events[:50]
		}
		recorded = true
		return nil
	})
	if err == nil && recorded {
		s.wakeNotifications()
	}
	return err
}

// Only mismatches reach the store. Ordinary model declarations add no database
// reads or writes to the forwarding path. Each attempt/turn handles at most one.
func (s *OpenAIGatewayService) accountRiskModelCallback(ctx context.Context, groupID int64, account *Account, sentModel func() string) func(string) {
	if s == nil || s.accountRisk == nil || account == nil || account.Platform != PlatformOpenAI || groupID <= 0 {
		return nil
	}
	handled := false
	return func(responseModel string) {
		sent := strings.TrimSpace(sentModel())
		if handled || sent == "" || upstreamModelsMatchForAudit(sent, responseModel) {
			return
		}
		handled = true
		// A client disconnect must not discard already observed risk evidence.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			err = s.accountRisk.HandleModelMismatch(persistCtx, groupID, account, sent, responseModel, time.Now().UTC())
			if !errors.Is(err, ErrAccountRiskBusy) && !errors.Is(err, errAccountRiskModelAccountChanged) {
				break
			}
			select {
			case <-persistCtx.Done():
				err = persistCtx.Err()
			case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
			}
			if persistCtx.Err() != nil {
				break
			}
		}
		if err != nil {
			slog.Warn("account model mismatch risk action failed", "group_id", groupID, "account_id", account.ID, "error", err)
		}
	}
}

func (s *OpenAIGatewayService) beginAccountRiskModelObservation(ctx context.Context, c *gin.Context, account *Account) *upstreamResponseModelObserver {
	if c != nil {
		ClearOpsUpstreamModel(c)
		callback := s.accountRiskModelCallback(ctx, getOpenAIGroupIDFromContext(c), account, func() string { return c.GetString(OpsUpstreamModelKey) })
		c.Set(accountRiskModelObserverKey, callback)
	}
	return beginUpstreamResponseModelObservation(c)
}

func (s *OpenAIGatewayService) newAccountRiskModelObserver(ctx context.Context, c *gin.Context, account *Account, sentModel string) *upstreamResponseModelObserver {
	return &upstreamResponseModelObserver{onModel: s.accountRiskModelCallback(ctx, getOpenAIGroupIDFromContext(c), account, func() string { return sentModel })}
}
