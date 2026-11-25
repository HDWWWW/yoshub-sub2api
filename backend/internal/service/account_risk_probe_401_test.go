//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRiskProbe401ActionsAndNotification(t *testing.T) {
	for _, tc := range []struct {
		name, body, refresh, action string
		invalidate                  bool
	}{
		{"revoked", `{"error":{"code":"token_revoked","message":"private-upstream-text"}}`, "rt", "stop", false},
		{"invalidated", `{"error":{"code":"token_invalidated"}}`, "rt", "stop", false},
		{"unauthorized", `{"detail":"Unauthorized"}`, "rt", "stop", false},
		{"no refresh", `{"detail":"token expired"}`, "", "stop", true},
		{"refreshable", `{"detail":"token expired"}`, "rt", "cooldown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Probe401Enabled = true
			r.Config.Notify401Enabled = true
			r.Config.ProbeNotifyOnly = true
			r.Config.NotifyEnabled = false // 401 alerts have their own switch.
			accounts.pool[0].Credentials = map[string]any{"access_token": "private-token", "refresh_token": tc.refresh}
			job := riskDueJob(t, s, accounts, now)
			upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(401, tc.body)}}
			repo := &openAIAccountTestRepo{}
			s.probe = &AccountTestService{httpUpstream: upstream, accountRepo: repo}
			notifier := &riskNoticeStub{}
			s.notifier = notifier
			callbacks := 0
			s.onProbe401Committed = func(_ context.Context, a *Account, until time.Time, invalidate bool) {
				callbacks++
				require.Equal(t, job.account.ID, a.ID)
				require.Equal(t, tc.action == "stop", until.IsZero())
				require.Equal(t, tc.invalidate, invalidate)
			}
			s.runProbe(context.Background(), job)
			require.Len(t, upstream.requests, 1)
			require.Zero(t, repo.setErrorID)
			require.Equal(t, 1, callbacks)
			require.Len(t, r.Events, 1)
			require.Equal(t, accountRiskProbe401, r.Events[0].Kind)
			require.Equal(t, tc.action, r.Events[0].Action)
			require.Equal(t, "pending", r.Events[0].Notification)
			if tc.action == "cooldown" {
				require.Equal(t, StatusActive, accounts.pool[0].Status)
				require.True(t, accounts.pool[0].Schedulable)
				require.WithinDuration(t, time.Now().Add(10*time.Minute), *accounts.pool[0].TempUnschedulableUntil, time.Second)
			} else {
				require.Equal(t, StatusError, accounts.pool[0].Status)
				require.False(t, accounts.pool[0].Schedulable)
			}
			pool, err := s.candidates(context.Background(), 7, now)
			require.NoError(t, err)
			require.Empty(t, pool)
			// A duplicate finish / another worker cannot publish the same event twice.
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{}, errors.New("duplicate"), now))
			require.Len(t, r.Events, 1)
			require.Equal(t, 1, callbacks)
			require.NoError(t, s.deliverNotification(context.Background(), now))
			require.NoError(t, s.deliverNotification(context.Background(), now.Add(time.Minute)))
			require.Equal(t, []int64{7}, notifier.groups)
			require.Len(t, notifier.messages, 1)
			require.Contains(t, notifier.messages[0], "401")
			require.NotContains(t, notifier.messages[0], "private-")
			require.Equal(t, "sent", r.Events[0].Notification)
			if tc.action == "cooldown" {
				require.Contains(t, notifier.messages[0], "冷却至 "+formatNotificationTime(*accounts.pool[0].TempUnschedulableUntil))
			} else {
				require.Contains(t, notifier.messages[0], "已关闭")
			}
		})
	}
}

func TestAccountRiskProbe401Guards(t *testing.T) {
	for _, scenario := range []string{"off", "non-401", "transport", "oversize", "credentials rotated", "rule edited", "stale membership", "lease expired", "write conflict", "manual"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Probe401Enabled = scenario != "off"
			accounts.pool[0].Credentials = map[string]any{"access_token": "fixture"}
			if scenario == "manual" {
				probe := s.probe.(*riskTestProbe)
				probe.err = errors.New("401")
				_, err := s.TestNow(context.Background(), 7, 1, r.Revision, r.Config)
				require.NoError(t, err)
				require.False(t, probe.lastConfig.Probe401Enabled)
				require.Empty(t, r.Events)
				return
			}
			job := riskDueJob(t, s, accounts, now)
			status := 401
			if scenario == "non-401" {
				status = 403
			}
			body := `{"error":{"code":"token_revoked"}}`
			if scenario == "oversize" {
				body += strings.Repeat(" ", 64<<10)
			}
			up := &queuedHTTPUpstream{}
			if scenario != "transport" {
				up.responses = []*http.Response{newJSONResponse(status, body)}
			}
			probe := &AccountTestService{httpUpstream: up, accountRepo: &openAIAccountTestRepo{}}
			result, err := probe.RunRiskProbe(context.Background(), &job.account, job.config)
			require.Error(t, err)
			switch scenario {
			case "credentials rotated":
				accounts.pool[0].Credentials = map[string]any{"access_token": "different"}
			case "rule edited":
				r.Revision++
			case "stale membership":
				accounts.pool = nil
			case "lease expired":
				now = job.pending.ExpiresAt
			case "write conflict":
				store.cas = false
			}
			require.NoError(t, s.finishProbe(context.Background(), job, result, err, now))
			require.Empty(t, r.Events)
			require.Empty(t, store.stopped)
		})
	}
}

func TestAccountRiskProbe401AllAndCapacity(t *testing.T) {
	for _, kind := range []string{"all", "capacity notify only", "paused retest"} {
		t.Run(kind, func(t *testing.T) {
			s, store, accounts, _, now := riskAllAccountsHarness()
			if kind != "all" {
				s, store, accounts, _, now = riskHarness()
			}
			r := store.state.Rules[7]
			r.Config.Probe401Enabled = true
			r.Config.Notify401Enabled = true
			r.Config.CapacityNotifyOnly = kind == "capacity notify only"
			r.Config.NotifyEnabled = true
			var job *accountRiskJob
			if kind == "all" {
				job = riskAllAccountsDueJobs(t, s, now)[0]
			} else {
				job = riskCapacityDueJob(t, s, now)
			}
			e := readRiskProbe401(&job.account, r.Config.Model, strings.NewReader(`{"error":{"code":"token_revoked"}}`))
			require.NotNil(t, e)
			err := s.finishProbe(context.Background(), job, AccountRiskProbeResult{UpstreamStatus: 401, auth401: e}, errors.New("rejected"), now.Add(12*time.Second))
			require.NoError(t, err)
			require.Equal(t, "stop", r.Events[0].Action)
			if kind == "paused retest" {
				require.Equal(t, "disabled", r.Events[0].Notification)
			} else {
				require.Equal(t, "pending", r.Events[0].Notification)
			}
			require.Equal(t, job.account.ID, r.Events[0].AccountID)
			require.Empty(t, store.state.CapacityPauses)
			target, err := accounts.GetByID(context.Background(), job.account.ID)
			require.NoError(t, err)
			require.Equal(t, StatusError, target.Status)
			if kind == "paused retest" {
				require.Equal(t, "cancelled", r.Events[1].Notification)
			}
		})
	}
}

func TestAccountRiskProbe401OnlyEditKeepsPausedRecheck(t *testing.T) {
	s, store, _, _, now := riskHarness()
	job := riskCapacityDueJob(t, s, now)
	r := store.state.Rules[7]
	require.NotEmpty(t, job.pauseToken)
	c := r.Config
	c.Probe401Enabled = true
	require.NoError(t, s.Configure(context.Background(), 7, r.Revision, c))
	require.Equal(t, r.Revision, store.state.CapacityPauses[job.account.ID].Revision)
	require.Error(t, s.validateProbeReservation(context.Background(), job))
}

func TestAccountRiskProbe401ShadowCredentialRotation(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		t.Run(map[bool]string{false: "current parent", true: "rotated parent"}[rotated], func(t *testing.T) {
			s, store, accounts, _, now := riskAllAccountsHarness()
			r := store.state.Rules[7]
			r.Config.Probe401Enabled = true
			parent := &accounts.pool[1]
			parent.Credentials = map[string]any{"access_token": "first", "refresh_token": "usable"}
			accounts.pool[0].ParentAccountID = &parent.ID
			jobs := riskAllAccountsDueJobs(t, s, now)
			var job *accountRiskJob
			for _, j := range jobs {
				if j.account.ID == accounts.pool[0].ID {
					job = j
				}
			}
			require.NotNil(t, job)
			e := readRiskProbe401(parent, r.Config.Model, strings.NewReader(`{"error":{"code":"token_revoked"}}`))
			require.NotNil(t, e)
			if rotated {
				parent.Credentials = map[string]any{"access_token": "rotated"}
			}
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{UpstreamStatus: 401, auth401: e}, errors.New("rejected"), now))
			require.Equal(t, StatusActive, accounts.pool[0].Status)
			if rotated {
				require.Empty(t, r.Events)
				require.Equal(t, StatusActive, parent.Status)
			} else {
				require.Equal(t, parent.ID, r.Events[0].AccountID)
				require.Equal(t, StatusError, parent.Status)
			}
		})
	}
}
