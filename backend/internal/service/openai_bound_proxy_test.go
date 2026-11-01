//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type boundProxyTestRepository struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r *boundProxyTestRepository) GetByID(context.Context, int64) (*Proxy, error) {
	return r.proxy, r.err
}

func unavailableBoundProxyRepositories() []struct {
	name string
	repo ProxyRepository
} {
	return []struct {
		name string
		repo ProxyRepository
	}{
		{name: "missing repository"},
		{name: "deleted proxy", repo: &boundProxyTestRepository{err: errors.New("proxy was deleted")}},
		{name: "missing proxy", repo: &boundProxyTestRepository{}},
	}
}

type boundProxyTestOAuthClient struct {
	OpenAIOAuthClient
	calls    int
	proxyURL string
	err      error
}

func (c *boundProxyTestOAuthClient) RefreshTokenWithClientID(_ context.Context, _, proxyURL, _ string) (*openai.TokenResponse, error) {
	c.calls++
	c.proxyURL = proxyURL
	return nil, c.err
}

func TestOpenAIRefreshAccountTokenUnavailableProxyNeverGoesDirect(t *testing.T) {
	for _, tc := range unavailableBoundProxyRepositories() {
		for _, mode := range []string{"refresh", "access token", "personal access token"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				client := &boundProxyTestOAuthClient{err: errors.New("upstream must not be called")}
				privacyCalls := 0
				svc := &OpenAIOAuthService{
					proxyRepo:   tc.repo,
					oauthClient: client,
					privacyClientFactory: func(string) (*req.Client, error) {
						privacyCalls++
						return nil, errors.New("upstream must not be called")
					},
				}
				id := int64(42)
				account := &Account{
					ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &id,
					Credentials: map[string]any{"access_token": "test-token"},
				}
				switch mode {
				case "refresh":
					account.Credentials["refresh_token"] = "test-refresh"
				case "personal access token":
					account.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
				}

				got, err := svc.RefreshAccountToken(context.Background(), account)
				require.Nil(t, got)
				require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
				require.Equal(t, "OPENAI_ACCOUNT_PROXY_UNAVAILABLE", infraerrors.Reason(err))
				require.Zero(t, client.calls)
				require.Zero(t, privacyCalls)
			})
		}
	}
}

func TestOpenAIRefreshAccountTokenPreservesExplicitEgress(t *testing.T) {
	proxy := &Proxy{ID: 42, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "bound proxy"}[bound], func(t *testing.T) {
			upstreamErr := errors.New("test upstream stop")
			client := &boundProxyTestOAuthClient{err: upstreamErr}
			svc := &OpenAIOAuthService{oauthClient: client}
			account := &Account{
				Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"refresh_token": "test-refresh"},
			}
			wantProxyURL := ""
			if bound {
				account.ProxyID = &proxy.ID
				svc.proxyRepo = &boundProxyTestRepository{proxy: proxy}
				wantProxyURL = proxy.URL()
			}

			_, err := svc.RefreshAccountToken(context.Background(), account)
			require.ErrorIs(t, err, upstreamErr)
			require.Equal(t, 1, client.calls)
			require.Equal(t, wantProxyURL, client.proxyURL)
		})
	}
}

func TestOpenAIQuotaUnavailableProxyNeverGoesDirect(t *testing.T) {
	for _, tc := range unavailableBoundProxyRepositories() {
		t.Run(tc.name, func(t *testing.T) {
			id := int64(42)
			account := &Account{
				ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &id,
				Credentials: map[string]any{"chatgpt_account_id": "test-account"},
			}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
			calls := 0
			svc := NewOpenAIQuotaService(repo, tc.repo, NewOpenAITokenProvider(repo, cache, nil), func(string) (*req.Client, error) {
				calls++
				return nil, errors.New("upstream must not be called")
			}, nil)

			got, err := svc.QueryUsage(context.Background(), account.ID)
			require.Nil(t, got)
			require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
			require.Equal(t, "OPENAI_ACCOUNT_PROXY_UNAVAILABLE", infraerrors.Reason(err))
			require.Zero(t, calls)
		})
	}
}

func TestOpenAIQuotaPreservesExplicitEgress(t *testing.T) {
	proxy := &Proxy{ID: 42, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	for _, mode := range []string{"direct", "loaded proxy", "resolved proxy"} {
		t.Run(mode, func(t *testing.T) {
			account := &Account{
				ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"chatgpt_account_id": "test-account"},
			}
			var proxyRepo ProxyRepository
			wantProxyURL := ""
			if mode != "direct" {
				account.ProxyID = &proxy.ID
				wantProxyURL = proxy.URL()
				if mode == "loaded proxy" {
					account.Proxy = proxy
				} else {
					proxyRepo = &boundProxyTestRepository{proxy: proxy}
				}
			}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
			calls := 0
			svc := NewOpenAIQuotaService(repo, proxyRepo, NewOpenAITokenProvider(repo, cache, nil), func(proxyURL string) (*req.Client, error) {
				calls++
				require.Equal(t, wantProxyURL, proxyURL)
				return nil, errors.New("test upstream stop")
			}, nil)

			_, err := svc.QueryUsage(context.Background(), account.ID)
			require.Equal(t, "OPENAI_QUOTA_CLIENT_ERROR", infraerrors.Reason(err))
			require.Equal(t, 1, calls)
		})
	}
}

func TestTokenRefreshPrivacyUnavailableProxyNeverGoesDirect(t *testing.T) {
	for _, tc := range unavailableBoundProxyRepositories() {
		for _, platform := range []string{PlatformOpenAI, PlatformAntigravity} {
			t.Run(tc.name+"/"+platform, func(t *testing.T) {
				id := int64(42)
				account := &Account{
					ID: 10, Platform: platform, Type: AccountTypeOAuth, ProxyID: &id,
					Credentials: map[string]any{"access_token": "test-token", "project_id": "test-project"},
				}
				repo := &stubQuotaAccountRepo{}
				calls := 0
				svc := &TokenRefreshService{
					accountRepo: repo, proxyRepo: tc.repo,
					privacyClientFactory: func(string) (*req.Client, error) {
						calls++
						return nil, errors.New("upstream must not be called")
					},
				}
				// A canceled context also keeps a regression in Antigravity's
				// internally constructed HTTP client from contacting the network.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if platform == PlatformOpenAI {
					svc.ensureOpenAIPrivacy(ctx, account)
				} else {
					svc.ensureAntigravityPrivacy(ctx, account)
				}
				require.Zero(t, calls)
				require.Zero(t, repo.extraUpdateCalls)
				require.Empty(t, account.Extra)
			})
		}
	}
}

func TestTokenRefreshPrivacyPreservesExplicitEgress(t *testing.T) {
	proxy := &Proxy{ID: 42, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "bound proxy"}[bound], func(t *testing.T) {
			account := &Account{
				ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token"},
			}
			repo := &stubQuotaAccountRepo{}
			calls := 0
			wantProxyURL := ""
			svc := &TokenRefreshService{accountRepo: repo}
			if bound {
				account.ProxyID = &proxy.ID
				svc.proxyRepo = &boundProxyTestRepository{proxy: proxy}
				wantProxyURL = proxy.URL()
			}
			svc.privacyClientFactory = func(proxyURL string) (*req.Client, error) {
				calls++
				require.Equal(t, wantProxyURL, proxyURL)
				return nil, errors.New("test upstream stop")
			}

			svc.ensureOpenAIPrivacy(context.Background(), account)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, repo.extraUpdateCalls)
			require.Equal(t, PrivacyModeFailed, repo.extraUpdates[account.ID]["privacy_mode"])
		})
	}
}
