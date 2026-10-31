//go:build unit

package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The receiving gateway uses its existing stream loops and TTFT calculation.
// HAPPY_NEWAPI_PROXY optionally inserts the unchanged deployed NewAPI handler
// from the external compatibility harness. No downstream timing code is patched.
func TestHappyFirstTokenDownstreamResponses(t *testing.T) {
	previousCache := gatewayForwardingCache.Load()
	if previousCache == nil {
		previousCache = &cachedGatewayForwardingSettings{}
	}
	t.Cleanup(func() { gatewayForwardingCache.Store(previousCache) })
	for _, hop := range []string{"direct", "unchanged_newapi"} {
		for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
			for _, passthrough := range []bool{false, true} {
				name := hop + "/" + mode + "/native"
				if passthrough {
					name = hop + "/" + mode + "/passthrough"
				}
				t.Run(name, func(t *testing.T) {
					proxy := os.Getenv("HAPPY_NEWAPI_PROXY")
					if hop == "unchanged_newapi" && proxy == "" {
						t.Skip("set HAPPY_NEWAPI_PROXY to the unchanged NewAPI verification adapter")
					}
					sourceResult := make(chan *openaiStreamingResult, 1)
					sourceError := make(chan error, 1)
					router := gin.New()
					router.POST("/v1/responses", func(c *gin.Context) {
						reader, writer := io.Pipe()
						defer reader.Close()
						go func() {
							defer writer.Close()
							time.Sleep(400 * time.Millisecond)
							_, _ = io.WriteString(writer, happySuccessStream)
						}()
						upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
						result, err := happyRunStream(happyTestService(t, 30), c, upstream, false, time.Now())
						sourceResult <- result
						sourceError <- err
					})
					source := httptest.NewServer(router)
					defer source.Close()
					target := source.URL + "/v1/responses"
					if hop == "unchanged_newapi" {
						target = proxy + "/relay?upstream=" + url.QueryEscape(target) + "&id=" + url.QueryEscape(t.Name())
					}
					started := time.Now()
					client := &http.Client{Timeout: 5 * time.Second}
					response, err := client.Post(target, "application/json", strings.NewReader(`{}`))
					require.NoError(t, err)
					defer response.Body.Close()
					gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: mode, expiresAt: time.Now().Add(time.Minute).UnixNano()})
					downstream := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
					c, output := happyTestContext(t)
					result, err := happyRunStream(downstream, c, response, passthrough, started)
					require.NoError(t, err)
					require.NoError(t, <-sourceError)
					original := <-sourceResult
					require.NotNil(t, original.firstTokenMs)
					require.GreaterOrEqual(t, *original.firstTokenMs, 350, "source logging must still measure the actual upstream")
					require.NotNil(t, result.firstTokenMs)
					if mode == OpenAITTFTModeSemantic {
						require.Less(t, *result.firstTokenMs, 250, "unchanged downstream semantic TTFT must see the real status frame")
					} else {
						require.GreaterOrEqual(t, *result.firstTokenMs, 350, "visible TTFT cannot be shortened by a content-free event")
					}
					require.EqualValues(t, 5, result.usage.InputTokens)
					require.EqualValues(t, 2, result.usage.OutputTokens)
					require.Equal(t, 1, strings.Count(output.Body.String(), `"type":"gateway.status"`))
					require.Equal(t, 1, strings.Count(output.Body.String(), `"delta":"hello"`))
					require.Equal(t, 1, strings.Count(output.Body.String(), `"type":"response.completed"`))
					t.Logf("source_ttft_ms=%d downstream_%s_ttft_ms=%d", *original.firstTokenMs, mode, *result.firstTokenMs)
					if hop == "unchanged_newapi" {
						var data map[string]any
						require.Eventually(t, func() bool {
							metrics, err := client.Get(proxy + "/result?id=" + url.QueryEscape(t.Name()))
							if err != nil {
								return false
							}
							defer metrics.Body.Close()
							return metrics.StatusCode == http.StatusOK && json.NewDecoder(metrics.Body).Decode(&data) == nil
						}, time.Second, 10*time.Millisecond)
						require.Less(t, data["first_response_ms"].(float64), float64(250))
						require.NotContains(t, data, "failure_status")
						t.Logf("unchanged_newapi=%v", data)
					}
				})
			}
		}
	}
}

func TestHappyFirstTokenDownstreamFailureBoundary(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, failure := range []string{
			`{"type":"response.failed","response":{"status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limited"}}}`,
			`{"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Context length exceeded"}}}`,
		} {
			c, output := happyTestContext(t)
			gateway := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(openAIHappyFirstTokenEvent + "data: " + failure + "\n\n"))}
			_, err := happyRunStream(gateway, c, response, passthrough, time.Now())
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover, "an unchanged downstream commits an extension event; retry is no longer safe")
			require.Contains(t, output.Body.String(), `"type":"response.failed"`)
			require.Contains(t, output.Body.String(), "exceeded")
			require.NotContains(t, output.Body.String(), "response.completed")
		}
	}
	// Keep the negative mode guarantee explicit even when external integration
	// tests are not configured: no status event may pretend to contain text.
	require.False(t, openAIStreamDataStartsVisibleOutput(`{"type":"gateway.status","metadata":{"gateway_status":"upstream_connected"}}`, "gateway.status"))
}
