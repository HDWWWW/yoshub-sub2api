//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func allRiskNotificationTypes() *AccountRiskNotificationTypes {
	return &AccountRiskNotificationTypes{Capacity: true, Business429: true, CustomError: true,
		ScheduledMismatch: true, Scheduled429: true, Auth401: true, ModelMismatch: true}
}

func TestAccountRiskNotificationTypeFilters(t *testing.T) {
	cases := []struct {
		kind  string
		types AccountRiskNotificationTypes
	}{
		{accountRiskProbeCapacity, AccountRiskNotificationTypes{Capacity: true}},
		{"rate_limit_429", AccountRiskNotificationTypes{Business429: true}},
		{"custom_error", AccountRiskNotificationTypes{CustomError: true}},
		{accountRiskProbeSingle, AccountRiskNotificationTypes{ScheduledMismatch: true}},
		{accountRiskProbeAll, AccountRiskNotificationTypes{ScheduledMismatch: true}},
		{accountRiskProbeSingle + "_429", AccountRiskNotificationTypes{Scheduled429: true}},
		{accountRiskProbeAll + "_429", AccountRiskNotificationTypes{Scheduled429: true}},
		{accountRiskProbeSingle + "_mismatch_429", AccountRiskNotificationTypes{Scheduled429: true}},
		{accountRiskProbeAll + "_mismatch_429", AccountRiskNotificationTypes{Scheduled429: true}},
		{accountRiskProbe401, AccountRiskNotificationTypes{Auth401: true}},
		{accountRiskBusiness401, AccountRiskNotificationTypes{Auth401: true}},
		{accountRiskAccount401, AccountRiskNotificationTypes{Auth401: true}},
		{"model_mismatch", AccountRiskNotificationTypes{ModelMismatch: true}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			c := DefaultAccountRiskConfig()
			c.Enabled, c.NotifyEnabled, c.Notify401Enabled = true, true, true
			c.CapacityNotifyOnly, c.ProbeNotifyOnly, c.ModelMismatchAction = true, true, "notify_only"
			for _, action := range []string{"stop", "notify_only", riskActionReduceConcurrency} {
				event := AccountRiskEvent{Kind: tc.kind, Action: action}
				c.NotificationTypes = nil
				require.True(t, riskEventNotificationEnabled(c, event), "legacy behavior")
				c.NotificationTypes = &AccountRiskNotificationTypes{}
				require.False(t, riskEventNotificationEnabled(c, event))
				c.NotificationTypes = &tc.types
				require.True(t, riskEventNotificationEnabled(c, event))
			}
		})
	}
}

func TestAccountRiskNotificationConfigRoundTripAndValidation(t *testing.T) {
	c := DefaultAccountRiskConfig()
	require.Equal(t, "wecom", riskNotificationChannel(c))
	for _, channel := range []string{"", "wecom", "qq"} {
		c.NotificationChannel = channel
		require.NoError(t, c.Validate())
	}
	c.NotificationChannel = "both"
	require.ErrorContains(t, c.Validate(), "通知渠道")
	c.NotificationChannel, c.NotificationTypes = "qq", &AccountRiskNotificationTypes{}
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	var decoded AccountRiskConfig
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, c, decoded)
	require.NotNil(t, decoded.NotificationTypes, "explicit all-off filter must survive persistence")
	require.NoError(t, json.Unmarshal([]byte(`{"notify_enabled":true}`), &decoded))
	require.Nil(t, decoded.NotificationTypes)
	require.Equal(t, "wecom", riskNotificationChannel(decoded))
}

func TestAccountRiskNotificationUntypedLegacyQueueHonorsExclusions(t *testing.T) {
	c := DefaultAccountRiskConfig()
	c.NotificationTypes = allRiskNotificationTypes()
	require.True(t, riskNotificationTypeEnabled(c, AccountRiskEvent{}))
	c.NotificationTypes.Auth401 = false
	require.False(t, riskNotificationTypeEnabled(c, AccountRiskEvent{}))
}

func TestAccountRiskNotificationSelectionIsClaimedAndNotReplayed(t *testing.T) {
	for _, channel := range []string{"", "wecom", "qq"} {
		t.Run(channel, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			rule := store.state.Rules[7]
			rule.Config.NotifyEnabled, rule.Config.NotificationChannel = true, channel
			n := &riskNoticeStub{err: errors.New("uncertain delivery")}
			n.after = func() {
				require.Equal(t, "sending", rule.Events[0].Notification)
				rule.Config.NotificationChannel = "wecom"
			}
			s.notifier = n
			riskRecordStop(rule, &accounts.pool[0], now, "risk", accountRiskProbeSingle)
			require.NoError(t, s.deliverNotification(context.Background(), now))
			expected := channel
			if expected == "" {
				expected = "wecom"
			}
			require.Equal(t, []string{expected}, n.channels)
			require.Equal(t, expected, rule.Events[0].NotificationChannel)
			require.Equal(t, "failed", rule.Events[0].Notification)
			restarted := &AccountRiskService{store: store, groups: s.groups, notifier: n}
			require.NoError(t, restarted.deliverNotification(context.Background(), now.Add(time.Hour)))
			require.Len(t, n.messages, 1)
		})
	}
}

func TestAccountRiskNotificationFilterCancelsQueuedMessages(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	rule := store.state.Rules[7]
	rule.Config.NotifyEnabled = true
	n := &riskNoticeStub{}
	s.notifier = n
	riskRecordStop(rule, &accounts.pool[0], now, "risk", accountRiskProbeSingle)
	rule.Config.NotificationTypes = &AccountRiskNotificationTypes{Business429: true}
	require.NoError(t, s.deliverNotification(context.Background(), now))
	require.Equal(t, "cancelled", rule.Events[0].Notification)
	require.Empty(t, n.messages)
	// A future event of a selected type remains deliverable.
	appendAccountRiskAction(rule, &accounts.pool[0], AccountRiskEvent{At: now.Add(time.Second), Kind: "rate_limit_429", Action: riskActionReduceConcurrency})
	require.NoError(t, s.deliverNotification(context.Background(), now.Add(4*time.Second)))
	require.Len(t, n.messages, 1)
	require.Equal(t, "sent", rule.Events[0].Notification)
}

func TestAccountRiskNotificationFiltersDoNotSuppressSchedulingActions(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	rule := store.state.Rules[7]
	rule.Config.NotifyEnabled = true
	rule.Config.NotificationTypes = &AccountRiskNotificationTypes{}
	job := riskDueJob(t, s, accounts, now)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(301*time.Second)))
	require.Equal(t, []int64{1}, store.stopped)
	require.Equal(t, "disabled", rule.Events[0].Notification)
	// A filtered 401 remains in the history and never becomes pending.
	appendRisk401Event(rule, 1, "account", now, accountRiskAccount401, "observed", "401", true)
	require.Equal(t, "disabled", rule.Events[0].Notification)
}

func TestAccountRiskNotificationOnlyEditsPreserveCapacityPauses(t *testing.T) {
	s, store, _, _, _ := riskHarness()
	rule := store.state.Rules[7]
	store.state.CapacityPauses = map[int64]*AccountRiskCapacityPause{1: {GroupID: 7, Revision: rule.Revision}}
	previous := rule.Observation
	c := rule.Config
	c.NotificationChannel, c.NotificationTypes = "qq", &AccountRiskNotificationTypes{Auth401: true}
	require.NoError(t, s.Configure(context.Background(), 7, rule.Revision, c))
	require.Equal(t, rule.Revision, store.state.CapacityPauses[1].Revision)
	require.Equal(t, previous, rule.Observation)
}

func TestAccountRiskQQReusesGroupSecretsIndependentlyOfRefillChannel(t *testing.T) {
	for _, target := range []string{"group", "c2c"} {
		t.Run(target, func(t *testing.T) {
			s, store, supplier, _ := refillHarness(t)
			c := refillTestConfig()
			c.Enabled, c.MonitorEnabled = false, false
			c.Notifications = qqRefillConfig()
			c.Notifications.QQTargetType = target
			c.Notifications.RefillStarted = false
			require.NoError(t, s.ConfigureWithNotificationSecrets(context.Background(), c, "", 1, AccountRefillNotificationSecrets{QQClientSecret: "risk-secret", WeComWebhook: fixtureRefillWebhook}))
			c.Notifications.Channel = "wecom"
			require.NoError(t, s.Configure(context.Background(), c, "", 2))
			before := store.raw
			requests := 0
			s.qqNotifier = &qqRefillNotifier{client: &http.Client{Transport: qqRefillRoundTrip(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.Path == "/app/getAppAccessToken" {
					return qqRefillResponse(200, `{"access_token":"risk-token","expires_in":7200}`), nil
				}
				path := "/v2/groups/"
				if target == "c2c" {
					path = "/v2/users/"
				}
				require.Equal(t, path+c.Notifications.QQTargetOpenID+"/messages", req.URL.Path)
				var payload map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
				require.Equal(t, "risk alert", payload["content"])
				return qqRefillResponse(200, `{"id":"risk-message"}`), nil
			})}}
			channels, err := s.RiskNotificationChannels(context.Background(), c.GroupID)
			require.NoError(t, err)
			require.Equal(t, map[string]bool{"wecom": true, "qq": true}, channels)
			require.NoError(t, s.SendRiskNotification(context.Background(), c.GroupID, "qq", "risk alert"))
			require.Equal(t, 2, requests)
			require.Equal(t, before, store.raw)
			require.Empty(t, supplier.submitted)
			store.mutate(t, func(st *accountRefillState) { st.EncryptedQQSecret = "" })
			channels, err = s.RiskNotificationChannels(context.Background(), c.GroupID)
			require.NoError(t, err)
			require.False(t, channels["qq"])
			require.True(t, channels["wecom"])
			require.Error(t, s.SendRiskNotification(context.Background(), c.GroupID, "qq", "not sent"))
			require.Error(t, s.SendRiskNotification(context.Background(), c.GroupID, "invalid", "not sent"))
			require.Equal(t, 2, requests, "missing QQ must not fall back or send")
		})
	}
}
