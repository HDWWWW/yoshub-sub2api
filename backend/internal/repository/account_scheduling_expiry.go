package repository

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Identifiers are supplied only by repository code/Ent, never request input.
// The native expiry branch remains indexable; grace is bounded and import-only.
func accountSchedulingExpirySQL(expires, platform, kind, extra string) string {
	raw := extra + "->>'" + service.AccountImportExpiryGraceExtraKey + "'"
	return fmt.Sprintf(`(%s + CASE WHEN %s = 'openai' AND %s = 'oauth'
		AND %s ~ '^[0-9]{1,5}$'
		THEN CASE WHEN (%s)::integer BETWEEN 1 AND 86400
			THEN (%s)::integer * INTERVAL '1 second' ELSE INTERVAL '0 seconds' END
		ELSE INTERVAL '0 seconds' END)`, expires, platform, kind, raw, raw, raw)
}

func aliasedAccountSchedulingExpirySQL(alias string) string {
	if alias != "" {
		alias += "."
	}
	return accountSchedulingExpirySQL(alias+"expires_at", alias+"platform", alias+"type", alias+"extra")
}
