//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNotificationTimeBeijingAcrossMidnight(t *testing.T) {
	const expected = "2026-11-25 07:20:39 (UTC+08:00)"
	for _, stamp := range []string{"2026-11-24T23:20:39Z", "2026-11-25T07:20:39+08:00", "2026-11-24T15:20:39-08:00"} {
		instant, err := time.Parse(time.RFC3339, stamp)
		require.NoError(t, err)
		require.Equal(t, expected, formatNotificationTime(instant))
	}
	require.Equal(t, "—", formatNotificationTime(time.Time{}))
	at := time.Date(2026, 11, 24, 23, 20, 39, 0, time.UTC)
	s, store, accounts, _, _ := riskHarness()
	notifier := &riskNoticeStub{}
	s.notifier = notifier
	rule := store.state.Rules[7]
	rule.Config.NotifyEnabled = true
	riskRecordStop(rule, &accounts.pool[0], at, "mismatch")
	require.NoError(t, s.deliverNotification(context.Background(), at.Add(time.Hour)))
	require.Len(t, notifier.messages, 1)
	require.Contains(t, notifier.messages[0], "北京时间："+expected)
	require.Equal(t, at, rule.Events[0].At)

	legacy := AccountRiskEvent{Kind: accountRiskProbe401, Action: "cooldown", Reason: "自动风控探测收到上游 HTTP 401，凭据刷新冷却至 2026-11-24T23:20:39Z"}
	require.Equal(t, "自动风控探测收到上游 HTTP 401，凭据刷新冷却至 "+expected, riskNotificationReason(legacy))
}

func TestNotificationEmailTimestampsMatchTemplatesAndFallbacks(t *testing.T) {
	at := time.Date(2026, 11, 24, 23, 20, 39, 0, time.UTC)
	const expected = "2026-11-25 07:20:39 (UTC+08:00)"
	event := &OpsAlertEvent{FiredAt: at}
	log := &ContentModerationLog{CreatedAt: at}
	cfg := &ContentModerationConfig{}
	require.Equal(t, expected, opsAlertEmailVariables(nil, event)["triggered_at"])
	require.Equal(t, expected, contentModerationEmailVariables(log, cfg)["triggered_at"])
	for _, body := range []string{
		buildOpsAlertEmailBody(&OpsAlertRule{}, event),
		buildOpsSummaryEmailHTML("summary", at, at.Add(time.Hour), &OpsDashboardOverview{}),
		buildOpsAccountHealthEmailHTML("health", at, at.Add(time.Hour), nil),
		buildContentModerationViolationEmailBody("test", log, cfg),
		buildContentModerationAccountDisabledEmailBody("test", log, cfg),
		buildCyberPolicyNoticeEmailBody("test", log),
	} {
		require.Contains(t, body, expected)
		require.NotContains(t, body, "(UTC)")
	}
	digest := buildOpsErrorDigestEmailHTML("digest", at.Add(-time.Hour), at.Add(time.Hour), &OpsErrorLogList{Errors: []*OpsErrorLog{{CreatedAt: at}}})
	require.Contains(t, digest, "<td>"+expected+"</td>")
	for _, locale := range []string{"zh", "en"} {
		vars := notificationEmailSampleVariables(locale)
		rendered, err := renderNotificationEmail(NotificationEmailEventOpsScheduledReport, "report", "<p>{{report_end_time}} (UTC)</p><p>machine logs (UTC)</p>", vars, nil)
		require.NoError(t, err)
		require.Contains(t, rendered.HTML, "2026-07-19 09:00:26 (UTC+08:00)</p>")
		require.Contains(t, rendered.HTML, "machine logs (UTC)")
	}
}
