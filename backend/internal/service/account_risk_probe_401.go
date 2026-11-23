package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const accountRiskProbe401 = "probe_401"

// Only an upstream HTTP response produces evidence. Never retain the body,
// tokens, or a provider-controlled string in the rule, logs, or notification.
type riskProbe401Evidence struct {
	ownerID  int64
	identity [32]byte
	failure  credential401Failure
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
	return &riskProbe401Evidence{ownerID: owner.ID, identity: riskModelAccountIdentity(owner), failure: classifyCredential401(owner, body)}
}

// Apply the response only to the credential version that actually sent the probe.
// The account update, event and scheduler outbox commit in one risk transaction.
func (s *AccountRiskService) applyProbe401(ctx context.Context, r *AccountRiskRule, scheduling AccountRiskScheduling, job *accountRiskJob, evidence *riskProbe401Evidence, now time.Time) (*Account, time.Time, error) {
	stale := "账号、凭据或分组已变化，本次 401 结果作废"
	source, err := s.probeAccount(ctx, job, now)
	if err != nil {
		r.Observation.LastProbeResult = stale
		return nil, time.Time{}, nil
	}
	owner := source
	if source.IsShadow() {
		if *source.ParentAccountID != evidence.ownerID {
			r.Observation.LastProbeResult = stale
			return nil, time.Time{}, nil
		}
		owner, err = s.accounts.GetByID(ctx, evidence.ownerID)
		if err != nil {
			return nil, time.Time{}, err
		}
	}
	if owner == nil || owner.ID != evidence.ownerID || !owner.IsActive() || riskModelAccountIdentity(owner) != evidence.identity {
		r.Observation.LastProbeResult = stale
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
		reason += fmt.Sprintf("，凭据刷新冷却至 %s", blockedUntil.Format(time.RFC3339))
	}
	changed, err := scheduling.ApplyProbe401(r.GroupID, source, owner, reason, until)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !changed {
		r.Observation.LastProbeResult = stale
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
	event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: owner.ID, AccountName: riskNotificationName(owner.Name), Reason: r.Observation.LastProbeResult, Kind: accountRiskProbe401, Action: action, Notification: "pending"}
	r.Events = append([]AccountRiskEvent{event}, r.Events...)
	trimAccountRiskEvents(r)
	return owner, blockedUntil, nil
}
