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
const risk401ReclaimNote = "，已到号商预计回收时间，本次提醒已静默"

// Only an upstream HTTP response produces evidence. Never retain the body,
// tokens, or a provider-controlled string in the rule, logs, or notification.
type riskProbe401Evidence struct {
	alertTarget *Account401AlertTarget
	receivedAt  time.Time
	reclaimAt   *time.Time
	ownerID     int64
	ownerName   string
	ownerEmail  string
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
	// An in-flight probe can also carry a deadline from its original lease.
	if t := e.alertTarget; t != nil && e.reclaimAt != nil && (t.ReclaimAt == nil || e.reclaimAt.Before(*t.ReclaimAt)) {
		t.ReclaimAt = e.reclaimAt
	}
}

func (e *riskProbe401Evidence) alertsEnabled(r *AccountRiskRule) bool {
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
	return &riskProbe401Evidence{ownerID: owner.ID, ownerName: riskNotificationName(owner.Name), ownerEmail: riskAccountEmail(owner), identity: riskModelAccountIdentity(owner), failure: classifyCredential401(owner, body)}
}

// Apply the response only to the credential version that actually sent the probe.
// The account update, event and scheduler outbox commit in one risk transaction.
func (s *AccountRiskService) applyProbe401(ctx context.Context, r *AccountRiskRule, scheduling AccountRiskScheduling, job *accountRiskJob, evidence *riskProbe401Evidence, now time.Time) (*Account, time.Time, error) {
	if evidence.failure == "" || !r.Config.Probe401Enabled {
		r.Observation.LastProbeResult = "自动风控探测收到上游 HTTP 401"
		if evidence.alertsEnabled(r) {
			return nil, time.Time{}, appendProbe401Event(r, scheduling, job, evidence, now, "notify_only", r.Observation.LastProbeResult, false)
		}
		return nil, time.Time{}, nil
	}
	stale := "自动风控探测收到上游 HTTP 401，账号、凭据或分组已变化，未执行停调"
	alertStale := func() error {
		r.Observation.LastProbeResult = stale
		if evidence.alertsEnabled(r) {
			return appendProbe401Event(r, scheduling, job, evidence, now, "notify_only", stale, false)
		}
		return nil
	}
	source, err := s.probeAccount(ctx, job, now)
	if err != nil {
		return nil, time.Time{}, alertStale()
	}
	owner := source
	if source.IsShadow() {
		if *source.ParentAccountID != evidence.ownerID {
			return nil, time.Time{}, alertStale()
		}
		owner, err = s.accounts.GetByID(ctx, evidence.ownerID)
		if err != nil {
			return nil, time.Time{}, err
		}
	}
	if owner == nil || owner.ID != evidence.ownerID || !owner.IsActive() || riskModelAccountIdentity(owner) != evidence.identity {
		return nil, time.Time{}, alertStale()
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
		return nil, time.Time{}, alertStale()
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
	return owner, blockedUntil, appendProbe401Event(r, scheduling, job, evidence, now, action, r.Observation.LastProbeResult, false)
}

// The reservation owns one terminal 401, including a late result saved after a rule edit.
func appendProbe401Event(r *AccountRiskRule, tx AccountRiskScheduling, job *accountRiskJob, evidence *riskProbe401Evidence, now time.Time, action, reason string, reclaimed bool) error {
	id := job.pending.ID + "/401"
	if slices.ContainsFunc(r.Events, func(e AccountRiskEvent) bool { return e.ID == id }) {
		return nil
	}
	at := evidence.observedAt(now)
	reclaimed = reclaimed || supplierReclaimed(evidence.reclaimAt, at) || (evidence.alertTarget != nil && supplierReclaimed(evidence.alertTarget.ReclaimAt, at))
	if reclaimed {
		reason += risk401ReclaimNote
	}
	event := AccountRiskEvent{ID: id, At: at, AccountID: evidence.ownerID, AccountName: evidence.ownerName,
		AccountEmail: evidence.ownerEmail, Kind: accountRiskProbe401, Action: action, Reason: reason}
	return appendRisk401Event(r, tx, evidence.ownerID, event, evidence.alertsEnabled(r) && !reclaimed)
}

// Claim the first delivery and persist the audit row in the same transaction as
// any probe action. A group filter or a reclaimed lease never consumes a claim.
func appendRisk401Event(r *AccountRiskRule, tx AccountRiskScheduling, ownerID int64, event AccountRiskEvent, notify bool) error {
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	event.AccountName = riskNotificationName(event.AccountName)
	if event.Notification == "" {
		event.Notification = "disabled"
	}
	if notify && riskNotificationTypeEnabled(r.Config, event) {
		first, count, err := tx.Record401(r.GroupID, ownerID, event.ID, event.At)
		if err != nil {
			return err
		}
		event.Occurrences = count
		event.Notification = "merged"
		if first == event.ID && count == 1 {
			event.Notification = "pending"
		} else if count > 1 {
			for i := range r.Events {
				if r.Events[i].ID == first {
					r.Events[i].Occurrences = count
					break
				}
			}
		}
	}
	r.Events = append([]AccountRiskEvent{event}, r.Events...)
	trimAccountRiskEvents(r)
	return nil
}
