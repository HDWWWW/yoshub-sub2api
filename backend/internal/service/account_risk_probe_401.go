package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
)

const accountRiskProbe401 = "probe_401"

// Only an upstream HTTP response produces evidence. Never retain the body,
// tokens, or a provider-controlled string in the rule, logs, or notification.
type riskProbe401Evidence struct {
	alertTarget *Account401AlertTarget
	receivedAt  time.Time
	reclaimAt   *time.Time
	ownerID     int64
	ownerName   string
	identity    [32]byte
	failure     credential401Failure
}

func (e *riskProbe401Evidence) observedAt(fallback time.Time) time.Time {
	if !e.receivedAt.IsZero() {
		return e.receivedAt
	}
	return fallback
}

func (e *riskProbe401Evidence) captureAlertTarget(ctx context.Context, reporter *Account401Reporter, sourceID int64) {
	if e.alertTarget == nil {
		e.alertTarget = reporter.targetAt401(ctx, sourceID, e.ownerID)
	}
}

func (e *riskProbe401Evidence) shouldNotify(r *AccountRiskRule) bool {
	return r.Config.Notify401Enabled && e.alertTarget != nil && e.alertTarget.AccountID > 0
}

func readRiskProbe401(owner *Account, model string, reader io.Reader) *riskProbe401Evidence {
	if owner == nil || owner.Platform != PlatformOpenAI || reader == nil || !owner.ShouldHandleErrorCode(http.StatusUnauthorized) || (owner.IsPoolMode() && !owner.IsCustomErrorCodesEnabled()) {
		return nil
	}
	const maxBody = 64 << 10
	body, err := io.ReadAll(io.LimitReader(reader, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil
	}
	// Request/model-scoped failures and an expired Agent Identity task are not
	// evidence against the account's reusable credentials.
	if isOpenAIContentPolicyRejection(body) || isOpenAICyberAccessDenied(http.StatusUnauthorized, body) ||
		(owner.IsOpenAIAgentIdentity() && isAgentIdentityTaskInvalidHTTPResponse(http.StatusUnauthorized, body)) ||
		(owner.Type == AccountTypeAPIKey && isOpenAICompatibleModelNotFoundBody(body)) ||
		tempUnschedRulesApply(owner, http.StatusUnauthorized, body, model) {
		return nil
	}
	return &riskProbe401Evidence{ownerID: owner.ID, ownerName: riskNotificationName(owner.Name), identity: riskModelAccountIdentity(owner), failure: classifyCredential401(owner, body)}
}

// Apply the response only to the credential version that actually sent the probe.
// The account update, event and scheduler outbox commit in one risk transaction.
func (s *AccountRiskService) applyProbe401(ctx context.Context, r *AccountRiskRule, scheduling AccountRiskScheduling, job *accountRiskJob, evidence *riskProbe401Evidence, now time.Time) (*Account, time.Time, error) {
	if evidence.failure == "" || !r.Config.Probe401Enabled {
		r.Observation.LastProbeResult = "自动风控探测收到上游 HTTP 401"
		if evidence.shouldNotify(r) {
			appendProbe401Event(r, job, evidence, now, "notify_only", r.Observation.LastProbeResult)
		}
		return nil, time.Time{}, nil
	}
	stale := "自动风控探测收到上游 HTTP 401，账号、凭据或分组已变化，未执行停调"
	alertStale := func() {
		r.Observation.LastProbeResult = stale
		if evidence.shouldNotify(r) {
			appendProbe401Event(r, job, evidence, now, "notify_only", stale)
		}
	}
	source, err := s.probeAccount(ctx, job, now)
	if err != nil {
		alertStale()
		return nil, time.Time{}, nil
	}
	owner := source
	if source.IsShadow() {
		if *source.ParentAccountID != evidence.ownerID {
			alertStale()
			return nil, time.Time{}, nil
		}
		owner, err = s.accounts.GetByID(ctx, evidence.ownerID)
		if err != nil {
			return nil, time.Time{}, err
		}
	}
	if owner == nil || owner.ID != evidence.ownerID || !owner.IsActive() || riskModelAccountIdentity(owner) != evidence.identity {
		alertStale()
		return nil, time.Time{}, nil
	}
	reason := fmt.Sprintf("自动风控探测收到上游 HTTP 401（%s）", evidence.failure)
	if owner.ID != source.ID {
		reason += fmt.Sprintf("，探测账号 #%d 使用此账号的凭据", source.ID)
	}
	action := "stop"
	var until *time.Time
	var blockedUntil time.Time
	if evidence.failure == credential401Refreshable {
		blockedUntil = now.Add(oauth401Cooldown(s.cfg))
		until = &blockedUntil
		action = "cooldown"
		reason += fmt.Sprintf("，凭据刷新冷却至 %s", formatNotificationTime(blockedUntil))
	}
	changed, err := scheduling.ApplyProbe401(r.GroupID, source, owner, reason, until)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !changed {
		alertStale()
		return nil, time.Time{}, nil
	}
	r.Observation.LastProbeResult = reason
	if action == "stop" {
		r.Observation.LastProbeResult += "，已关闭账号调度"
		riskReset(&r.Observation, "stopped", r.Observation.LastProbeResult)
	}
	// A permanent credential failure supersedes a pending capacity-pause notice.
	if until == nil {
		for i := range r.Events {
			e := &r.Events[i]
			if e.AccountID == owner.ID && e.Kind == accountRiskProbeCapacity && e.Action == "pause" && e.Notification == "pending" {
				e.Notification = "cancelled"
			}
		}
	}
	appendProbe401Event(r, job, evidence, now, action, r.Observation.LastProbeResult)
	return owner, blockedUntil, nil
}

// The reservation owns one terminal 401, including a late result saved after a rule edit.
func appendProbe401Event(r *AccountRiskRule, job *accountRiskJob, evidence *riskProbe401Evidence, now time.Time, action, reason string) {
	id := job.pending.ID + "/401"
	if slices.ContainsFunc(r.Events, func(e AccountRiskEvent) bool { return e.ID == id }) {
		return
	}
	appendRisk401Event(r, evidence.ownerID, evidence.ownerName, evidence.observedAt(now), accountRiskProbe401, action, reason, evidence.shouldNotify(r))
	r.Events[0].ID = id
}

func appendRisk401Event(r *AccountRiskRule, id int64, name string, now time.Time, kind, action, reason string, notify bool) {
	notification := "disabled"
	if notify {
		notification = "pending"
	}
	event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: id, AccountName: riskNotificationName(name), Reason: reason, Kind: kind, Action: action, Notification: notification}
	filterRiskNotification(r.Config, &event)
	r.Events = append([]AccountRiskEvent{event}, r.Events...)
	trimAccountRiskEvents(r)
}
