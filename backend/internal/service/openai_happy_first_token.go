package service

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// This extension describes the gateway connection, without inventing model
// output or account quota. Existing Responses relays count and forward unknown
// events, unlike response.metadata. Those relays consequently commit their
// stream; their later pre-output retry is unavailable. This gateway excludes
// its own notification bytes from account-failover decisions.
const openAIHappyFirstTokenEvent = "event: gateway.status\ndata: {\"type\":\"gateway.status\",\"metadata\":{\"gateway_status\":\"upstream_connected\"}}\n\n"

const openAIHappyFirstTokenDeadlineKey = "openai_happy_first_token_deadline"

// Called only after an upstream HTTP success has opened a Responses SSE stream.
// HTTP authentication/rate-limit failures before this point keep their status.
func (s *OpenAIGatewayService) startOpenAIResponseSignals(ctx context.Context, c *gin.Context, account *Account, _ http.Header, started time.Time, heartbeat time.Duration, initialFrame ...[]byte) func() {
	var initialDelay *time.Duration
	// The notification describes this gateway, not a particular account. If it
	// wins the race with model output, the stream commits without attempt-bound
	// headers (including the optional Codex turn-state). The stream handlers keep
	// those headers staged, so a failed account's state can never escape or be
	// attributed to the successful account. Real output arriving first still
	// commits the original headers through the normal forwarding path.
	if c != nil && account != nil && account.Platform == PlatformOpenAI && account.IsOAuth() && !IsOpenAIResponsesCompactPath(c) && !openAICompactClientWantsStream(c) {
		settings := s.settingService.happyFirstTokenSettings(ctx)
		var groupID int64
		if value, ok := c.Get("api_key"); ok {
			if key, ok := value.(*APIKey); ok && key != nil && key.GroupID != nil {
				groupID = *key.GroupID
			}
		}
		settings = settings.ForGroup(groupID)
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
	return startOpenAISSESignals(c, heartbeat, initialDelay, initialFrame...)
}
