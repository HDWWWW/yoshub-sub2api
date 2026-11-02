//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func forwardImageCancellationTest(t *testing.T, svc *OpenAIGatewayService, c *gin.Context, route string, stream bool) (*OpenAIForwardResult, error) {
	t.Helper()
	if route == "embeddings" {
		return svc.ForwardEmbeddings(c.Request.Context(), c, newOpenAIImagesAPIKeyAccount(), []byte(`{"model":"text-embedding-3-small","input":"hello"}`), "")
	}
	model := "gpt-image-2"
	account := newOpenAIImagesAPIKeyAccount()
	if route != "apikey" {
		account = directImagesTestAccount()
	}
	if route == "responses" {
		model = "gpt-image-1"
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","stream":%t}`, model, stream))
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	if err != nil {
		return nil, err
	}
	return svc.ForwardImages(c.Request.Context(), c, account, body, parsed, "")
}

func imageCancellationPartialPayload(route string) string {
	if route == "embeddings" {
		return `{"data":[`
	}
	if route == "responses" {
		return "data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"AA==\",\"partial_image_index\":0,\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n\n"
	}
	return "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"AA==\",\"partial_image_index\":0,\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n\n"
}

func TestOpenAIImageEmbeddingCancellationReachesHTTPUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"apikey", "direct", "responses", "embeddings"} {
		for _, h2 := range []bool{false, true} {
			for _, phase := range []string{"before_headers", "body", "error_body", "write_failure"} {
				if route == "embeddings" && phase == "write_failure" {
					continue
				}
				t.Run(fmt.Sprintf("%s/h2=%v/%s", route, h2, phase), func(t *testing.T) {
					started := make(chan struct{})
					canceled := make(chan error, 1)
					shutdown := make(chan struct{})
					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if (r.ProtoMajor == 2) != h2 {
							canceled <- fmt.Errorf("unexpected protocol %s", r.Proto)
							close(started)
							return
						}
						if phase != "before_headers" {
							w.Header().Set("Content-Type", "text/event-stream")
							payload := imageCancellationPartialPayload(route)
							if phase == "error_body" {
								w.WriteHeader(http.StatusTooManyRequests)
								payload = `{"error":{"message":"`
							}
							_, _ = io.WriteString(w, payload)
							w.(http.Flusher).Flush()
						}
						close(started)
						select {
						case <-r.Context().Done():
							canceled <- r.Context().Err()
						case <-shutdown:
						}
					}))
					server.EnableHTTP2 = h2
					server.StartTLS()
					t.Cleanup(server.Close)
					t.Cleanup(func() { close(shutdown) })
					upstreamURL, err := url.Parse(server.URL)
					require.NoError(t, err)
					upstream := &cancellationNetworkUpstream{client: server.Client(), url: upstreamURL}
					svc := newOpenAIImagesTestService(upstream)
					c, _ := newOpenAIImagesTestContext(t, nil)
					ctx, cancel := context.WithCancel(c.Request.Context())
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					if phase == "write_failure" {
						c.Writer = &failingOpenAIImageWriter{ResponseWriter: c.Writer}
					}
					finished := make(chan error, 1)
					go func() {
						_, err := forwardImageCancellationTest(t, svc, c, route, true)
						finished <- err
					}()
					select {
					case <-started:
					case <-time.After(3 * time.Second):
						t.Fatal("upstream did not start")
					}
					if phase != "write_failure" {
						cancel()
					}
					select {
					case err := <-finished:
						require.ErrorIs(t, err, context.Canceled)
						var failover *UpstreamFailoverError
						require.NotErrorAs(t, err, &failover)
					case <-time.After(3 * time.Second):
						t.Fatal("forward did not stop on cancellation")
					}
					select {
					case err := <-canceled:
						require.ErrorIs(t, err, context.Canceled)
					case <-time.After(3 * time.Second):
						t.Fatal("upstream did not observe cancellation")
					}
					require.EqualValues(t, 1, upstream.calls.Load())
				})
			}
		}
	}
}

func TestOpenAIImageEmbeddingCancellationKeepsCompletedJSONUsage(t *testing.T) {
	for _, route := range []string{"apikey", "direct", "responses", "embeddings"} {
		t.Run(route, func(t *testing.T) {
			payload := `{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":7,"output_tokens":3}}`
			if route == "responses" {
				payload = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3},\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\"}]}}\n\n"
			}
			if route == "embeddings" {
				payload = `{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":7}}`
			}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
			svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: resp})
			c, _ := newOpenAIImagesTestContext(t, nil)
			c.Writer = &failingOpenAIImageWriter{ResponseWriter: c.Writer}
			result, err := forwardImageCancellationTest(t, svc, c, route, false)
			require.ErrorIs(t, err, context.Canceled)
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 7, result.Usage.InputTokens)
			if route != "embeddings" {
				require.Equal(t, 1, result.ImageCount)
			}
		})
	}
}

type imagePartialCancellationBody struct {
	data string
}

func (b *imagePartialCancellationBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, context.Canceled
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (*imagePartialCancellationBody) Close() error { return nil }

func TestOpenAIImagesCancellationPreservesReceivedUsageAndCompletedImages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, route := range []string{"apikey", "direct", "responses"} {
			t.Run(fmt.Sprintf("%s/stream=%v", route, stream), func(t *testing.T) {
				payload := `{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":7,"output_tokens":3}}`
				if stream {
					payload = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n\n"
				}
				if route == "responses" {
					payload = "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"id\":\"img_1\",\"result\":\"AA==\"},\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n\n"
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &imagePartialCancellationBody{data: payload}}
				svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: resp})
				c, rec := newOpenAIImagesTestContext(t, nil)
				result, err := forwardImageCancellationTest(t, svc, c, route, stream)
				require.ErrorIs(t, err, context.Canceled)
				require.NotNil(t, result)
				require.True(t, result.ClientDisconnect)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 3, result.Usage.OutputTokens)
				require.Equal(t, 1, result.ImageCount)
				if route == "responses" {
					require.NotContains(t, rec.Body.String(), "image_generation.completed")
				}
			})
		}
	}
}

func TestCodexDirectImagesCancellationDoesNotCountIncompleteJSON(t *testing.T) {
	payload := `{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":7,"output_tokens":3}`
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: &imagePartialCancellationBody{data: payload}}
	svc := newOpenAIImagesTestService(&httpUpstreamRecorder{resp: resp})
	c, _ := newOpenAIImagesTestContext(t, nil)
	result, err := forwardImageCancellationTest(t, svc, c, "direct", false)
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.Zero(t, result.ImageCount)
	require.Zero(t, result.Usage.InputTokens)
}
