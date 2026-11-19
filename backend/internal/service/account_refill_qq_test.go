//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type qqRefillRoundTrip func(*http.Request) (*http.Response, error)

func (f qqRefillRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func qqRefillResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func qqRefillConfig() AccountRefillNotificationConfig {
	return AccountRefillNotificationConfig{Channel: "qq", QQAppID: "123456", QQTargetType: "group", QQTargetOpenID: "GROUP_OPENID_FIXTURE_123", RefillStarted: true}
}

func TestRefillQQSendUsesOfficialAPIAndCachesToken(t *testing.T) {
	tokens, messages := 0, 0
	n := &qqRefillNotifier{client: &http.Client{Transport: qqRefillRoundTrip(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https", r.URL.Scheme)
		require.Equal(t, "api.bot.qq.com", r.URL.Host)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if r.URL.Path == "/app/getAppAccessToken" {
			tokens++
			require.Equal(t, "123456", body["appId"])
			require.Equal(t, "secret-fixture", body["clientSecret"])
			require.Empty(t, r.Header.Get("Authorization"))
			return qqRefillResponse(200, `{"access_token":"token-fixture","expires_in":"7200"}`), nil
		}
		messages++
		require.Equal(t, "QQBot token-fixture", r.Header.Get("Authorization"))
		require.Equal(t, float64(0), body["msg_type"])
		require.Equal(t, "测试通知", body["content"])
		require.NotContains(t, body, "msg_id")
		require.NotContains(t, body, "event_id")
		if messages == 1 {
			require.Equal(t, "/v2/groups/GROUP_OPENID_FIXTURE_123/messages", r.URL.Path)
		} else {
			require.Equal(t, "/v2/users/USER_OPENID_FIXTURE_123/messages", r.URL.Path)
		}
		return qqRefillResponse(200, `{"id":"msg-fixture"}`), nil
	})}}
	c := qqRefillConfig()
	require.NoError(t, n.Send(context.Background(), c, "secret-fixture", "测试通知"))
	c.QQTargetType, c.QQTargetOpenID = "c2c", "USER_OPENID_FIXTURE_123"
	require.NoError(t, n.Send(context.Background(), c, "secret-fixture", "测试通知"))
	require.Equal(t, 1, tokens)
	require.Equal(t, 2, messages)
}

func TestRefillQQErrorsAreRedactedAndAmbiguousDeliveryIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		network bool
		retry   bool
	}{
		{"permission", 400, `{"code":40034105,"message":"secret-fixture token-fixture"}`, false, false},
		{"rate", 429, `{"code":40034100}`, false, true},
		{"auth", 401, `{"code":100007}`, false, true},
		{"server", 503, `{"code":500}`, false, false},
		{"missing-id", 200, `{}`, false, false},
		{"invalid-json", 200, `bad secret-fixture`, false, false},
		{"disconnect", 0, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n := &qqRefillNotifier{client: &http.Client{Transport: qqRefillRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/app/getAppAccessToken" {
					return qqRefillResponse(200, `{"access_token":"token-fixture","expires_in":7200}`), nil
				}
				if tc.network {
					return nil, errors.New("secret-fixture transport failure")
				}
				return qqRefillResponse(tc.status, tc.body), nil
			})}}
			err := n.Send(context.Background(), qqRefillConfig(), "secret-fixture", "test")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-fixture")
			require.NotContains(t, err.Error(), "token-fixture")
			var result *qqRefillDeliveryError
			require.ErrorAs(t, err, &result)
			require.Equal(t, tc.retry, result.retryable)
			require.Equal(t, 2, calls, "never replay inside a send attempt")
		})
	}
}

func TestRefillQQConfigureEncryptsSecretAndTestDoesNotPurchase(t *testing.T) {
	s, store, provider, _ := refillHarness(t)
	c := refillTestConfig()
	c.Enabled = false
	c.Notifications = qqRefillConfig()
	ctx := context.Background()
	require.Error(t, s.Configure(ctx, c, "", 1))
	require.NoError(t, s.ConfigureWithNotificationSecrets(ctx, c, "", 1, AccountRefillNotificationSecrets{QQClientSecret: "secret-fixture"}))
	require.NotContains(t, store.raw, "secret-fixture")
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, v.QQSecretConfigured)
	public, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(public), "encrypted_qq_secret")
	require.NotContains(t, string(public), "ciphertext")
	require.NoError(t, s.Configure(ctx, c, "", 2), "blank preserves saved secret")
	c.Notifications.QQAppID = "654321"
	require.ErrorContains(t, s.Configure(ctx, c, "", 3), "更换 AppID")
	c.Notifications.QQAppID = "123456"
	requests := 0
	s.qqNotifier = &qqRefillNotifier{client: &http.Client{Transport: qqRefillRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path == "/app/getAppAccessToken" {
			return qqRefillResponse(200, `{"access_token":"token-fixture","expires_in":7200}`), nil
		}
		return qqRefillResponse(200, `{"id":"test-message"}`), nil
	})}}
	require.NoError(t, s.TestNotification(ctx, 3))
	require.ErrorContains(t, s.TestNotification(ctx, 3), "30 秒")
	require.Equal(t, 2, requests)
	require.Empty(t, provider.submitted)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.Jobs)
	require.Contains(t, state.NotificationStatus.Message, "QQ 已确认")
	store.mutate(t, func(st *accountRefillState) { st.LastNotificationTest = time.Time{} })
	store.failSave = true
	require.Error(t, s.TestNotification(ctx, 3), "failed persistence must prevent delivery")
	require.Equal(t, 2, requests)
}

func TestRefillQQConfigRejectsInvalidDestinations(t *testing.T) {
	for _, id := range []string{"12345678901234567", "../groups/123456789", "bad id with spaces", "short"} {
		c := qqRefillConfig()
		c.QQTargetOpenID = id
		require.Error(t, validateRefillQQConfig(c))
	}
	c := qqRefillConfig()
	c.QQAppID = "not-numeric"
	require.Error(t, validateRefillQQConfig(c))
	c = qqRefillConfig()
	c.QQTargetType = "channel"
	require.Error(t, validateRefillQQConfig(c))
	require.Error(t, validateRefillQQSecret("secret\r\nAuthorization: value"))
	require.NoError(t, validateRefillQQConfig(AccountRefillNotificationConfig{}), "legacy WeCom settings remain valid")
}

func TestRefillQQUnknownDemandDeliveryIsNotReplayed(t *testing.T) {
	s, store, provider, _ := refillHarness(t)
	ctx := context.Background()
	c := refillTestConfig()
	c.Enabled = false
	c.Notifications = qqRefillConfig()
	c.Notifications.RefillNeeded = true
	require.NoError(t, s.ConfigureWithNotificationSecrets(ctx, c, "", 1, AccountRefillNotificationSecrets{QQClientSecret: "secret-fixture"}))
	messages := 0
	s.qqNotifier = &qqRefillNotifier{client: &http.Client{Transport: qqRefillRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/app/getAppAccessToken" {
			return qqRefillResponse(200, `{"access_token":"token-fixture","expires_in":7200}`), nil
		}
		messages++
		persisted, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.True(t, persisted.RefillNeededRetryAt.IsZero(), "persist at-most-once before sending")
		return nil, errors.New("connection lost after sending")
	})}}
	for i := 0; i < 2; i++ {
		store.mutate(t, func(st *accountRefillState) {
			st.NextNeededCheckAt = time.Time{}
			st.ProviderRetryAt = time.Now().Add(time.Hour)
		})
		require.NoError(t, s.run(ctx, false))
	}
	require.Equal(t, 1, messages)
	require.Empty(t, provider.submitted)
	st, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, st.NotificationStatus.NoRetry)
	require.True(t, st.RefillNeededRetryAt.IsZero())
}
