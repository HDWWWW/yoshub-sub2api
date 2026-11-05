package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestExcelOAuthTokenRequest(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{true: "refresh", false: "exchange"}[refresh], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "true", r.URL.Query().Get("unified"))
				require.Equal(t, openai.ExcelClientID, r.PostForm.Get("client_id"))
				require.Empty(t, r.Header.Get("originator"))
				require.NotContains(t, r.Header.Get("User-Agent"), "codex")
				require.Empty(t, r.PostForm.Get("scope"))
				if refresh {
					require.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
				} else {
					require.Equal(t, openai.ExcelRedirectURI, r.PostForm.Get("redirect_uri"))
					require.Equal(t, "verifier", r.PostForm.Get("code_verifier"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"rotated","expires_in":3600}`))
			}))
			defer srv.Close()
			s := &openaiOAuthService{tokenURL: srv.URL}
			var result *openai.TokenResponse
			var err error
			if refresh {
				result, err = s.RefreshTokenWithClientID(context.Background(), "old", "", openai.ExcelClientID)
			} else {
				result, err = s.ExchangeCode(context.Background(), "code", "verifier", openai.ExcelRedirectURI, "", openai.ExcelClientID)
			}
			require.NoError(t, err)
			require.Equal(t, "access", result.AccessToken)
		})
	}
}
