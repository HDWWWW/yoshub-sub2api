//go:build unit

package service

import (
	"context"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type concurrentPrivacyRepo struct {
	AccountRepository
	mu                  sync.Mutex
	account             Account
	savedModes          []string
	changeBeforeOneSave bool
}

func (r *concurrentPrivacyRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.account
	account.Extra = maps.Clone(account.Extra)
	account.Credentials = maps.Clone(account.Credentials)
	return &account, nil
}

func (r *concurrentPrivacyRepo) UpdateOpenAIPrivacyMode(_ context.Context, _ int64, enabled bool, mode string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changeBeforeOneSave {
		r.changeBeforeOneSave = false
		r.account.Extra[OpenAIPrivacyEnabledExtraKey] = !enabled
	}
	if OpenAIPrivacyEnabled(r.account.Extra) != enabled {
		return false, nil
	}
	r.account.Extra["privacy_mode"] = mode
	r.savedModes = append(r.savedModes, mode)
	return true, nil
}

func (r *concurrentPrivacyRepo) setPreference(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account.Extra[OpenAIPrivacyEnabledExtraKey] = enabled
}

func newConcurrentPrivacyRepo() *concurrentPrivacyRepo {
	return &concurrentPrivacyRepo{account: Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-access", "refresh_token": "test-refresh"},
		Extra:       map[string]any{},
	}}
}

func TestOpenAIPrivacyLateEnsureAndRefreshCorrectCurrentPreference(t *testing.T) {
	for _, caller := range []string{"import ensure", "background ensure", "oauth enrichment"} {
		t.Run(caller, func(t *testing.T) {
			repo := newConcurrentPrivacyRepo()
			stale, err := repo.GetByID(context.Background(), 7)
			require.NoError(t, err)
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			var trainingAllowed atomic.Bool
			factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPatch {
					if calls.Add(1) == 1 {
						close(started)
						<-release
					}
					trainingAllowed.Store(request.URL.Query().Get("value") == "true")
				}
				_, _ = w.Write([]byte(`{}`))
			})
			svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: factory}
			finished := make(chan string, 1)
			go func() {
				switch caller {
				case "import ensure":
					finished <- svc.EnsureOpenAIPrivacy(context.Background(), stale)
				case "background ensure":
					refresh := &TokenRefreshService{accountRepo: repo, privacyClientFactory: factory}
					refresh.ensureOpenAIPrivacy(context.Background(), stale)
					latest, _ := repo.GetByID(context.Background(), 7)
					finished <- latest.Extra["privacy_mode"].(string)
				case "oauth enrichment":
					oauth := &OpenAIOAuthService{oauthClient: privacyPreferenceOAuthClient{}, privacyClientFactory: factory, privacyAccountRepo: repo}
					info, err := oauth.RefreshAccountToken(context.Background(), stale)
					if err != nil {
						finished <- "error: " + err.Error()
						return
					}
					finished <- info.PrivacyMode
				}
			}()
			<-started
			// The user disables privacy while the original enable call is in flight.
			repo.setPreference(false)
			fresh, err := repo.GetByID(context.Background(), 7)
			require.NoError(t, err)
			require.Equal(t, PrivacyModeTrainingOn, svc.ForceOpenAIPrivacy(context.Background(), fresh))
			close(release)
			require.Equal(t, PrivacyModeTrainingOn, <-finished)
			require.EqualValues(t, 3, calls.Load(), "the late call must actively correct upstream even though the new outcome was already saved")
			require.True(t, trainingAllowed.Load())
			require.Equal(t, []string{PrivacyModeTrainingOn, PrivacyModeTrainingOn}, repo.savedModes)
			current, err := repo.GetByID(context.Background(), 7)
			require.NoError(t, err)
			require.False(t, OpenAIPrivacyEnabled(current.Extra))
			require.Equal(t, PrivacyModeTrainingOn, current.Extra["privacy_mode"])
		})
	}
}

func TestOpenAIPrivacyChangedBetweenReadAndWriteIsNotPublished(t *testing.T) {
	repo := newConcurrentPrivacyRepo()
	repo.changeBeforeOneSave = true
	account, err := repo.GetByID(context.Background(), 7)
	require.NoError(t, err)
	requests := make(chan string, 3)
	factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, request *http.Request) {
		requests <- request.URL.Query().Get("value")
		w.WriteHeader(http.StatusOK)
	})
	svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: factory}
	require.Equal(t, PrivacyModeTrainingOn, svc.ForceOpenAIPrivacy(context.Background(), account))
	require.Equal(t, "false", <-requests)
	require.Equal(t, "true", <-requests)
	require.Empty(t, requests)
	require.Equal(t, []string{PrivacyModeTrainingOn}, repo.savedModes)
}

func TestOpenAIPrivacyEnsureReloadsPreferenceBeforeSkipping(t *testing.T) {
	repo := newConcurrentPrivacyRepo()
	repo.account.Extra["privacy_mode"] = PrivacyModeTrainingOff
	stale, err := repo.GetByID(context.Background(), 7)
	require.NoError(t, err)
	repo.setPreference(false)
	requests := make(chan string, 1)
	factory := privacyPreferenceFactory(t, func(w http.ResponseWriter, request *http.Request) {
		requests <- request.URL.Query().Get("value")
		w.WriteHeader(http.StatusOK)
	})
	svc := &adminServiceImpl{accountRepo: repo, privacyClientFactory: factory}
	require.Equal(t, PrivacyModeTrainingOn, svc.EnsureOpenAIPrivacy(context.Background(), stale))
	require.Equal(t, "true", <-requests)
}

func TestOpenAIPrivacyLockedMergePreservesConcurrentIntentAndOutcome(t *testing.T) {
	current := &Account{Extra: map[string]any{
		OpenAIPrivacyEnabledExtraKey: false,
		"privacy_mode":               PrivacyModeTrainingOn,
	}}
	for _, explicit := range []bool{false, true} {
		ctx := context.Background()
		if explicit {
			ctx = context.WithValue(ctx, openAIPrivacyPreferenceWriteKey{}, true)
		}
		stale := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			OpenAIPrivacyEnabledExtraKey: true,
			"privacy_mode":               PrivacyModeTrainingOff,
			"unrelated":                  "updated",
		}}
		MergeLockedOpenAIPrivacy(ctx, stale, current)
		require.Equal(t, explicit, OpenAIPrivacyEnabled(stale.Extra))
		require.Equal(t, PrivacyModeTrainingOn, stale.Extra["privacy_mode"], "only an upstream result can replace the observed outcome")
		require.Equal(t, "updated", stale.Extra["unrelated"])
	}
}
