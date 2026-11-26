//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRiskNotificationEmailValidation(t *testing.T) {
	for _, tc := range []struct{ name, email, want string }{
		{"Pro account", " owner@example.test ", "owner@example.test"},
		{"fallback@example.test", "", "fallback@example.test"},
		{"Pro account", "", ""},
		{"Pro account", "not-an-email", ""},
		{"Pro account", "Owner <owner@example.test>", ""},
		{"Pro account", "owner@example.test\n账号ID:123", ""},
		{"Pro account", strings.Repeat("x", 250) + "@example.test", ""},
	} {
		t.Run(tc.email+tc.name, func(t *testing.T) {
			a := &Account{Name: tc.name, Credentials: map[string]any{"email": tc.email, "access_token": "never-export-token"}}
			require.Equal(t, tc.want, riskAccountEmail(a))
		})
	}
	require.Empty(t, riskAccountEmail(nil))
	require.Empty(t, riskAccountEmail(&Account{Name: "Pro", Credentials: map[string]any{"email": map[string]any{"token": "private"}}}))
}

func TestAccountRiskNotificationIncludesCapturedEmailAndID(t *testing.T) {
	for _, channel := range []string{"wecom", "qq"} {
		t.Run(channel, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			rule := store.state.Rules[7]
			rule.Config.NotifyEnabled, rule.Config.NotificationChannel = true, channel
			accounts.pool[0].Name = "Pro account"
			accounts.pool[0].Credentials = map[string]any{"email": "original@example.test", "access_token": "never-export-token"}
			riskRecordStop(rule, &accounts.pool[0], now, "risk", accountRiskProbeSingle)
			raw, err := json.Marshal(rule.Events[0])
			require.NoError(t, err)
			require.NotContains(t, string(raw), "never-export-token")
			require.NoError(t, json.Unmarshal(raw, &rule.Events[0]), "queued email survives restart")
			accounts.pool[0].Credentials["email"] = "replacement@example.test"
			accounts.pool[0].Name = "Replacement"
			n := &riskNoticeStub{}
			s.notifier = n
			require.NoError(t, s.deliverNotification(context.Background(), now))
			require.Len(t, n.messages, 1)
			require.Contains(t, n.messages[0], "账号 ID：#1\n账号名称：Pro account\n账号邮箱：original@example.test\n")
			require.NotContains(t, n.messages[0], "replacement@example.test")
			require.NotContains(t, n.messages[0], "never-export-token")
		})
	}
}

func TestAccountRiskNotificationLegacyEmailFallback(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"legacy@example.test", "legacy@example.test"}, {"Pro account", "未提供"}} {
		s, store, _, _, now := riskHarness()
		rule := store.state.Rules[7]
		rule.Config.NotifyEnabled = true
		rule.Events = []AccountRiskEvent{{ID: "legacy", At: now, AccountID: 123, AccountName: tc.name, Notification: "pending"}}
		n := &riskNoticeStub{}
		s.notifier = n
		require.NoError(t, s.deliverNotification(context.Background(), now))
		require.Contains(t, n.messages[0], "账号 ID：#123")
		require.Contains(t, n.messages[0], "账号邮箱："+tc.want+"\n")
	}
}

func TestAccountRisk401CapturesEmailAtReceipt(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	rule := store.state.Rules[7]
	rule.Config.Notify401Enabled = true
	accounts.pool[0].Name = "Pro account"
	accounts.pool[0].Credentials = map[string]any{"email": "receipt@example.test"}
	s.reporter401().record(context.Background(), 7, &accounts.pool[0], accountRiskBusiness401, "gateway", "401")
	require.Len(t, rule.Events, 1)
	require.Equal(t, "receipt@example.test", rule.Events[0].AccountEmail)
	evidence := readRiskProbe401(&accounts.pool[0], "gpt-6.1-sol", strings.NewReader(`{"error":{"code":"invalid_api_key"}}`))
	require.NotNil(t, evidence)
	require.Equal(t, "receipt@example.test", evidence.ownerEmail)
	evidence.alertTarget = &Account401AlertTarget{AccountID: 1}
	accounts.pool[0].Credentials["email"] = "changed@example.test"
	appendProbe401Event(rule, &accountRiskJob{pending: AccountRiskPending{ID: "probe"}}, evidence, now.Add(time.Second), "stop", "401")
	require.Equal(t, "receipt@example.test", rule.Events[0].AccountEmail)
}

func TestAccountRiskCapacityAndModelMismatchCaptureEmail(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	rule := store.state.Rules[7]
	rule.Config.NotifyEnabled, rule.Config.ModelMismatchAction = true, "notify_only"
	accounts.pool[0].Credentials = map[string]any{"email": "risk@example.test"}
	riskCapacityEvent(rule, &accounts.pool[0], now, "capacity", "pause")
	require.Equal(t, "risk@example.test", rule.Events[0].AccountEmail)
	require.NoError(t, s.HandleModelMismatch(context.Background(), 7, &accounts.pool[0], "gpt-6.1-sol", "different-model", now))
	require.Equal(t, "model_mismatch", rule.Events[0].Kind)
	require.Equal(t, "risk@example.test", rule.Events[0].AccountEmail)
}
