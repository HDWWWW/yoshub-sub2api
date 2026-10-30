package service

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// response.metadata is an existing non-output event understood by NewAPI.
// Do not synthesize codex.rate_limits: it would expose account quota or invent
// quota values, and older relays treat it as committed output, preventing retry.
const openAIHappyFirstTokenEvent = "event: response.metadata\ndata: {\"type\":\"response.metadata\",\"metadata\":{\"gateway_status\":\"upstream_connected\"}}\n\n"

const openAIHappyFirstTokenDeadlineKey = "openai_happy_first_token_deadline"

// Called only after an upstream HTTP success has opened a Responses SSE stream.
// HTTP authentication/rate-limit failures before this point keep their status.
func (s *OpenAIGatewayService) startOpenAIResponseSignals(ctx context.Context, c *gin.Context, account *Account, upstream http.Header, started time.Time, heartbeat time.Duration) func() {
	var initialDelay *time.Duration
	// A preamble commits HTTP headers before the winning account is known.
	// Preserve Codex turn-state staging/provenance when upstream supplies it;
	// neither forwarding a losing account's state nor dropping it is safe.
	if c != nil && account != nil && account.Platform == PlatformOpenAI && account.IsOAuth() && extractOpenAICodexTurnState(upstream) == "" && !IsOpenAIResponsesCompactPath(c) && !openAICompactClientWantsStream(c) {
		settings := s.settingService.happyFirstTokenSettings(ctx)
		if settings.Enabled {
			value, _ := c.Get(openAIHappyFirstTokenDeadlineKey)
			deadline, scheduled := value.(time.Time)
			if !scheduled {
				delayMS := settings.MinDelayMS
				if span := settings.MaxDelayMS - settings.MinDelayMS; span > 0 {
					delayMS += rand.IntN(span + 1)
				}
				deadline = started.Add(time.Duration(delayMS) * time.Millisecond)
				// Retrying an account must not redraw or restart this request's delay.
				c.Set(openAIHappyFirstTokenDeadlineKey, deadline)
			}
			delay := time.Until(deadline)
			if delay < 0 {
				delay = 0
			}
			initialDelay = &delay
		}
	}
	return startOpenAISSESignals(c, heartbeat, initialDelay)
}
