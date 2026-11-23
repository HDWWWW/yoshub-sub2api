package service

import (
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// The gateway and automatic probes interpret the same credential-level 401s.
// Values are also safe to include in operational events without upstream text.
type credential401Failure string

const (
	credential401Revoked        credential401Failure = "token_revoked"
	credential401Invalidated    credential401Failure = "token_invalidated"
	credential401Unauthorized   credential401Failure = "Unauthorized"
	credential401MissingRefresh credential401Failure = "refresh_token missing"
	credential401Refreshable    credential401Failure = "OAuth 401"
	credential401Invalid        credential401Failure = "invalid credentials"
)

func classifyCredential401(account *Account, body []byte) credential401Failure {
	if account.Platform == PlatformOpenAI {
		switch extractUpstreamErrorCode(body) {
		case "token_revoked":
			return credential401Revoked
		case "token_invalidated":
			return credential401Invalidated
		}
		if gjson.GetBytes(body, "detail").String() == "Unauthorized" {
			return credential401Unauthorized
		}
	}
	if account.Type != AccountTypeOAuth {
		return credential401Invalid
	}
	if strings.TrimSpace(account.GetCredential("refresh_token")) == "" {
		return credential401MissingRefresh
	}
	return credential401Refreshable
}

func oauth401Cooldown(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.RateLimit.OAuth401CooldownMinutes > 0 {
		return time.Duration(cfg.RateLimit.OAuth401CooldownMinutes) * time.Minute
	}
	return 10 * time.Minute
}
