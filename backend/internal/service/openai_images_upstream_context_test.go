package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func newOpenAIImagesTestContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

func newOpenAIImagesTestService(upstream HTTPUpstream) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
}

func newOpenAIImagesAPIKeyAccount() *Account {
	return &Account{
		ID:       31,
		Name:     "openai-apikey-images",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}
}

func openAIImagesJSONResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req_img_ctx"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"created":1710000000,"data":[{"b64_json":"aGVsbG8="}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`,
		)),
	}
}

// OpenAI follows caller cancellation, while other providers keep their detached
// accounting behavior. Verify pre-cancellation never starts an OpenAI request.
func TestForwardOpenAIImagesAPIKey_UpstreamCancellationScope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, platform := range []string{PlatformOpenAI, PlatformGrok} {
			t.Run(fmt.Sprintf("%s/stream=%v", platform, stream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-image-2","prompt":"draw a cat","stream":%t}`, stream))
				c, _ := newOpenAIImagesTestContext(t, body)
				recorder := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
				svc := newOpenAIImagesTestService(recorder)
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				account := newOpenAIImagesAPIKeyAccount()
				account.Platform = platform
				// Grok's process-wide cross-client mapping may map gpt-* to a
				// text model. This fixture tests cancellation, not model routing.
				account.Credentials["model_mapping"] = map[string]any{"gpt-image-2": "gpt-image-2"}
				result, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
				if platform == PlatformOpenAI {
					require.ErrorIs(t, err, context.Canceled)
					require.Nil(t, result)
					require.Nil(t, recorder.lastReq)
				} else {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 1, result.ImageCount)
					require.NoError(t, recorder.lastReq.Context().Err())
				}
			})
		}
	}
}

func TestDetachUpstreamContextSemantics(t *testing.T) {
	t.Run("detachUpstreamContext_always_detaches", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		detached, release := detachUpstreamContext(ctx)
		defer release()
		require.NoError(t, detached.Err())
	})

	t.Run("detachStreamUpstreamContext_keeps_cancel_when_not_streaming", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		same, release := detachStreamUpstreamContext(ctx, false)
		defer release()
		require.ErrorIs(t, same.Err(), context.Canceled,
			"非流式时该函数原样返回请求 context")
	})

	t.Run("detachStreamUpstreamContext_detaches_when_streaming", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		detached, release := detachStreamUpstreamContext(ctx, true)
		defer release()
		require.NoError(t, detached.Err())
	})
}
