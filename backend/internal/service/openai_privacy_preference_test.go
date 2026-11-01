//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func privacyPreferenceFactory(t *testing.T, handler http.HandlerFunc) PrivacyClientFactory {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	return func(string) (*req.Client, error) {
		return req.C().WrapRoundTripFunc(func(next req.RoundTripper) req.RoundTripFunc {
			return func(request *req.Request) (*req.Response, error) {
				request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
				return next.RoundTrip(request)
			}
		}), nil
	}
}

func TestOpenAIPrivacyPreferenceDefaultsAndCompletedModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   map[string]any
		enabled bool
		skip    bool
	}{
		{"legacy absent", nil, true, false},
		{"legacy protected", map[string]any{"privacy_mode": PrivacyModeTrainingOff}, true, true},
		{"legacy opt out", map[string]any{"privacy_mode": "training_opt_out"}, true, true},
		{"empty outcome", map[string]any{"privacy_mode": ""}, true, false},
		{"unknown outcome", map[string]any{"privacy_mode": "unknown"}, true, false},
		{"failed is not off", map[string]any{"privacy_mode": PrivacyModeFailed}, true, false},
		{"disabled pending", map[string]any{OpenAIPrivacyEnabledExtraKey: false, "privacy_mode": PrivacyModeTrainingOff}, false, false},
		{"disabled complete", map[string]any{OpenAIPrivacyEnabledExtraKey: false, "privacy_mode": PrivacyModeTrainingOn}, false, true},
		{"disabled failed", map[string]any{OpenAIPrivacyEnabledExtraKey: false, "privacy_mode": PrivacyModeFailed}, false, false},
		{"enabled pending", map[string]any{OpenAIPrivacyEnabledExtraKey: true, "privacy_mode": PrivacyModeTrainingOn}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.enabled, OpenAIPrivacyEnabled(tc.extra))
			require.Equal(t, tc.skip, shouldSkipOpenAIPrivacyEnsure(tc.extra))
		})
	}
}

func TestSetOpenAIPrivacyBidirectionalAndFailureStates(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, status := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusForbidden} {
			t.Run(strconv.FormatBool(enabled)+"/"+strconv.Itoa(status), func(t *testing.T) {
				requests := make(chan *http.Request, 1)
				factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, r *http.Request) {
					requests <- r
					if status == http.StatusForbidden {
						w.Header().Set("cf-mitigated", "challenge")
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{}`))
				})
				mode := setOpenAIPrivacy(context.Background(), factory, "test-access", "", enabled)
				request := <-requests
				require.Equal(t, http.MethodPatch, request.Method)
				require.Equal(t, "training_allowed", request.URL.Query().Get("feature"))
				require.Equal(t, strconv.FormatBool(!enabled), request.URL.Query().Get("value"))
				require.Equal(t, "Bearer test-access", request.Header.Get("Authorization"))
				wantMode := PrivacyModeTrainingOn
				if enabled {
					wantMode = PrivacyModeTrainingOff
				}
				if status == http.StatusInternalServerError {
					wantMode = PrivacyModeFailed
				} else if status == http.StatusForbidden {
					wantMode = PrivacyModeCFBlocked
				}
				require.Equal(t, wantMode, mode)
			})
		}
	}
}

type privacyPreferenceAccountRepo struct {
	AccountRepository
	account *Account
	updates int
}

func (r *privacyPreferenceAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func (r *privacyPreferenceAccountRepo) Update(_ context.Context, account *Account) error {
	r.account = account
	return nil
}
func (r *privacyPreferenceAccountRepo) UpdateExtra(_ context.Context, _ int64, values map[string]any) error {
	r.updates++
	if r.account.Extra == nil {
		r.account.Extra = make(map[string]any)
	}
	for key, value := range values {
		r.account.Extra[key] = value
	}
	return nil
}

func TestOpenAIPrivacyUpdatePersistsIntentAcrossImportsAndRetry(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-access"},
		Extra:       map[string]any{"privacy_mode": PrivacyModeTrainingOff}}
	repo := &privacyPreferenceAccountRepo{account: account}
	svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: func(string) (*req.Client, error) {
		return nil, errors.New("upstream unavailable")
	}}
	disabled := false
	updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{PrivacyEnabled: &disabled})
	require.NoError(t, err)
	require.Equal(t, false, updated.Extra[OpenAIPrivacyEnabledExtraKey])
	require.Equal(t, PrivacyModeFailed, updated.Extra["privacy_mode"])

	// A subsequent import/edit can replace Extra and carry a stale preference;
	// neither form may erase the explicitly saved false value.
	for _, extra := range []map[string]any{{}, {OpenAIPrivacyEnabledExtraKey: true}} {
		updated, err = svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Extra: extra})
		require.NoError(t, err)
		require.False(t, OpenAIPrivacyEnabled(updated.Extra))
	}

	requests := make(chan string, 4)
	factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("value")
		w.WriteHeader(http.StatusOK)
	})
	refresh := &TokenRefreshService{accountRepo: repo, privacyClientFactory: factory}
	refresh.ensureOpenAIPrivacy(context.Background(), updated)
	require.Equal(t, "true", <-requests)
	require.Equal(t, PrivacyModeTrainingOn, repo.account.Extra["privacy_mode"])
	refresh.ensureOpenAIPrivacy(context.Background(), updated)
	require.Empty(t, requests, "confirmed disabled preference must not be reversed or retried")

	svc.privacyClientFactory = factory
	enabled := true
	updated, err = svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{PrivacyEnabled: &enabled})
	require.NoError(t, err)
	require.Equal(t, "false", <-requests)
	require.Equal(t, PrivacyModeTrainingOff, updated.Extra["privacy_mode"])
}

func TestOpenAIPrivacyCreatePreferenceAndLegacyDefault(t *testing.T) {
	for _, setting := range []*bool{nil, privacyPreferenceBool(false), privacyPreferenceBool(true)} {
		input := &CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, PrivacyEnabled: setting}
		account, err := buildAccountForCreate(input, nil)
		require.NoError(t, err)
		require.Equal(t, setting == nil || *setting, OpenAIPrivacyEnabled(account.Extra))
		if setting != nil {
			require.Equal(t, *setting, account.Extra[OpenAIPrivacyEnabledExtraKey])
		}
	}
}

func TestOpenAIPrivacyImportEnsureKeepsExplicitFalse(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-access"},
		Extra:       map[string]any{OpenAIPrivacyEnabledExtraKey: false, "privacy_mode": PrivacyModeTrainingOff}}
	repo := &privacyPreferenceAccountRepo{account: account}
	requests := make(chan string, 1)
	svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: privacyPreferenceFactory(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("value")
		w.WriteHeader(http.StatusOK)
	})}
	require.Equal(t, PrivacyModeTrainingOn, svc.EnsureOpenAIPrivacy(context.Background(), account))
	require.Equal(t, "true", <-requests)
	require.False(t, OpenAIPrivacyEnabled(repo.account.Extra))
	require.Equal(t, PrivacyModeTrainingOn, repo.account.Extra["privacy_mode"])
}

func privacyPreferenceBool(value bool) *bool { return &value }

type privacyPreferenceOAuthClient struct{ OpenAIOAuthClient }

func (privacyPreferenceOAuthClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return &openai.TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, nil
}

func TestOpenAIPrivacyOAuthRefreshKeepsExplicitFalse(t *testing.T) {
	for _, hasRefreshToken := range []bool{true, false} {
		t.Run(strconv.FormatBool(hasRefreshToken), func(t *testing.T) {
			requests := make(chan string, 2)
			factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch {
					requests <- r.URL.Query().Get("value")
				}
				_, _ = w.Write([]byte(`{}`))
			})
			svc := &OpenAIOAuthService{oauthClient: privacyPreferenceOAuthClient{}, privacyClientFactory: factory}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "old-access"},
				Extra:       map[string]any{OpenAIPrivacyEnabledExtraKey: false}}
			if hasRefreshToken {
				account.Credentials["refresh_token"] = "old-refresh"
			}
			info, err := svc.RefreshAccountToken(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, "true", <-requests)
			require.Equal(t, PrivacyModeTrainingOn, info.PrivacyMode)
			require.Empty(t, requests)
		})
	}
}

func TestAccountRefillPrivacySnapshotSurvivesConfigChange(t *testing.T) {
	svc, store, provider, accounts := refillHarness(t)
	svc.provider = &manualRefillProvider{provider, 500}
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled, state.Config.MonitorEnabled = false, false
		state.Config.Delivery.PrivacyEnabled = privacyPreferenceBool(false)
	})
	require.NoError(t, svc.Manual(context.Background(), manualRequest()))
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Delivery.PrivacyEnabled = privacyPreferenceBool(true)
		require.NotNil(t, state.Jobs[0].DeliveryConfig.PrivacyEnabled)
		require.False(t, *state.Jobs[0].DeliveryConfig.PrivacyEnabled)
	})
	require.NoError(t, svc.run(context.Background(), false))
	provider.status = "succeeded"
	for range 2 {
		refillDue(t, store)
		require.NoError(t, svc.run(context.Background(), false))
	}
	require.Len(t, provider.submitted, 1)
	require.Equal(t, 1, accounts.creates)
	require.Equal(t, false, accounts.accounts[0].Extra[OpenAIPrivacyEnabledExtraKey])
	view, err := svc.View(context.Background())
	require.NoError(t, err)
	require.True(t, *view.Config.Delivery.PrivacyEnabled)
	require.False(t, *view.Jobs[0].DeliveryConfig.PrivacyEnabled)
}

func TestAccountRefillLegacyPrivacyDefault(t *testing.T) {
	var job AccountRefillJob
	require.NoError(t, json.Unmarshal([]byte(`{"delivery_config":{}}`), &job))
	require.Nil(t, job.DeliveryConfig.PrivacyEnabled)
	account, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, PrivacyEnabled: job.DeliveryConfig.PrivacyEnabled}, nil)
	require.NoError(t, err)
	require.True(t, OpenAIPrivacyEnabled(account.Extra))
}

func TestAccountRefillAutomaticPrivacySnapshotSurvivesConfigChange(t *testing.T) {
	svc, store, provider, accounts := refillHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Delivery.PrivacyEnabled = privacyPreferenceBool(false)
	})
	require.NoError(t, svc.run(context.Background(), false))
	require.Len(t, provider.submitted, 1)
	require.False(t, *provider.submitted[0].DeliveryConfig.PrivacyEnabled)
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled = false
		state.Config.Delivery.PrivacyEnabled = privacyPreferenceBool(true)
	})
	provider.status = "succeeded"
	for range 2 {
		refillDue(t, store)
		require.NoError(t, svc.run(context.Background(), false))
	}
	require.Len(t, provider.submitted, 1)
	require.Equal(t, 1, accounts.creates)
	require.Equal(t, false, accounts.accounts[0].Extra[OpenAIPrivacyEnabledExtraKey])
}
