package repository

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Identifiers are supplied only by repository code/Ent, never request input.
// Keep source lease precedence and bounded grace aligned with SchedulingExpiresAt.
func accountSchedulingExpirySQL(expires, platform, kind, extra string) string {
	raw := fmt.Sprintf("(CASE WHEN %s ? '%s' THEN %s->>'%s' ELSE %s->>'import_expiry_offset_seconds' END)", extra, service.AccountImportExpiryGraceExtraKey, extra, service.AccountImportExpiryGraceExtraKey, extra)
	source := expires
	// Keep validation and precedence aligned with SourceExpiresAt.
	for _, key := range []string{"import_original_expires_at", service.AccountSourceExpiryExtraKey} {
		value := extra + "->>'" + key + "'"
		source = fmt.Sprintf(`(CASE WHEN %s ~ '^[0-9]{1,12}$' THEN CASE WHEN (%s)::bigint BETWEEN 1 AND 253402214399 THEN LEAST(%s, to_timestamp((%s)::bigint)) ELSE %s END ELSE %s END)`, value, value, expires, value, source, source)
	}
	base := fmt.Sprintf("(CASE WHEN %s IS NULL THEN NULL WHEN %s = 'openai' AND %s = 'oauth' THEN %s ELSE %s END)", expires, platform, kind, source, expires)
	legacy := fmt.Sprintf(`(%s + CASE WHEN %s = 'openai' AND %s = 'oauth'
		AND %s ~ '^[0-9]{1,5}$'
		THEN CASE WHEN (%s)::integer BETWEEN 1 AND 86400
			THEN (%s)::integer * INTERVAL '1 second' ELSE INTERVAL '0 seconds' END
		ELSE INTERVAL '0 seconds' END)`, base, platform, kind, raw, raw, raw)
	return fmt.Sprintf("(CASE WHEN %s = 'openai' AND %s = 'oauth' AND %s->'%s' = 'true'::jsonb THEN %s ELSE %s END)", platform, kind, extra, service.AccountNativeExpiryExtraKey, expires, legacy)
}

func aliasedAccountSchedulingExpirySQL(alias string) string {
	if alias != "" {
		alias += "."
	}
	return accountSchedulingExpirySQL(alias+"expires_at", alias+"platform", alias+"type", alias+"extra")
}
