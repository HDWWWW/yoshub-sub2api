package service

import (
	"context"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type excelOAuthClient struct {
	OpenAIOAuthClient
	t        *testing.T
	response *openai.TokenResponse
	verifier string
}

func (m *excelOAuthClient) ExchangeCode(_ context.Context, code, verifier, redirect, proxy, client string) (*openai.TokenResponse, error) {
	require.Equal(m.t, "code", code)
	require.Equal(m.t, openai.ExcelClientID, client)
	require.Equal(m.t, openai.ExcelRedirectURI, redirect)
	require.NotEmpty(m.t, verifier)
	m.verifier = verifier
	return m.response, nil
}
func (m *excelOAuthClient) RefreshTokenWithClientID(_ context.Context, refresh, proxy, client string) (*openai.TokenResponse, error) {
	require.Equal(m.t, "old-refresh", refresh)
	require.Equal(m.t, openai.ExcelClientID, client)
	return m.response, nil
}

func TestExcelOAuthSessionAndRefresh(t *testing.T) {
	client := &excelOAuthClient{t: t, response: &openai.TokenResponse{AccessToken: testExcelBPSAccessToken(t, "excel-account"), RefreshToken: "rotated", ExpiresIn: 3600}}
	s := NewOpenAIOAuthService(nil, client)
	t.Cleanup(s.Stop)
	ctx := context.Background()
	result, err := s.GenerateAuthURL(ctx, nil, "http://localhost/wrong", "excel")
	require.NoError(t, err)
	u, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	require.Equal(t, "auth.openai.com", u.Host)
	require.Equal(t, "/api/accounts/authorize", u.Path)
	q := u.Query()
	require.Equal(t, openai.ExcelClientID, q.Get("client_id"))
	require.Equal(t, openai.ExcelRedirectURI, q.Get("redirect_uri"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	_, err = s.ExchangeCode(ctx, &OpenAIExchangeCodeInput{SessionID: result.SessionID, State: "wrong", Code: "code"})
	require.Error(t, err)
	info, err := s.ExchangeCode(ctx, &OpenAIExchangeCodeInput{SessionID: result.SessionID, State: q.Get("state"), Code: "code", RedirectURI: "http://localhost/wrong"})
	require.NoError(t, err)
	require.Equal(t, openai.GenerateCodeChallenge(client.verifier), q.Get("code_challenge"))
	require.Equal(t, "excel-account", info.ChatGPTAccountID)
	require.Equal(t, openai.ExcelClientID, info.ClientID)
	_, exists := s.sessionStore.Get(result.SessionID)
	require.False(t, exists)
	info, err = s.RefreshTokenWithClientID(ctx, "old-refresh", "", openai.ExcelClientID)
	require.NoError(t, err)
	require.Equal(t, "rotated", info.RefreshToken)
	client.response.RefreshToken = ""
	info, err = s.RefreshTokenWithClientID(ctx, "old-refresh", "", openai.ExcelClientID)
	require.NoError(t, err)
	require.Equal(t, "old-refresh", info.RefreshToken)
	require.Equal(t, "excel-account", info.ChatGPTAccountID)
}
