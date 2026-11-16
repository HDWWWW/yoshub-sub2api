package service

import (
	"errors"
	"net/http"
	"time"
)

const refillOrderPollInterval = 20 * time.Second

// Polling an accepted order never backs off based on total request count:
// successful status checks also increment Attempts. Keep purchase retries and
// provider metadata refreshes on their existing policy.
func nextRefillOrderPoll(err error) time.Time {
	delay := refillOrderPollInterval
	var remote *refillProviderError
	if errors.As(err, &remote) {
		delay = refillMax(delay, remote.retryAfter)
		if remote.code == http.StatusUnauthorized || remote.code == http.StatusForbidden {
			delay = refillMax(delay, 5*time.Minute)
		}
	}
	// Start the wait after the response/error. A slow request must not consume
	// the interval and cause an immediate retry when the worker wakes again.
	return time.Now().Add(delay)
}
