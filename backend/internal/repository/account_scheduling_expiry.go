package repository

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Use the first valid supplier/import timestamp, in the same order and range as
// Account.SourceExpiresAt. A local expiry is never a source timestamp.
func accountSourceExpirySQL(extra string) string {
	stamp := func(key string) string {
		value := extra + "->>'" + key + "'"
		return fmt.Sprintf(`CASE WHEN %[1]s ~ '^[0-9]{1,12}$' THEN CASE WHEN (%[1]s)::bigint BETWEEN 1 AND 253402214399 THEN to_timestamp((%[1]s)::bigint) END END`, value)
	}
	return "COALESCE(" + stamp(service.AccountSourceExpiryExtraKey) + ", " + stamp("import_original_expires_at") + ")"
}

// Identifiers are supplied only by repository code/Ent, never request input.
// Keep source lease precedence and bounded grace aligned with SchedulingExpiresAt.
func accountSchedulingExpirySQL(expires, platform, kind, extra string) string {
	raw := fmt.Sprintf("(CASE WHEN %s ? '%s' THEN %s->>'%s' ELSE %s->>'import_expiry_offset_seconds' END)", extra, service.AccountImportExpiryGraceExtraKey, extra, service.AccountImportExpiryGraceExtraKey, extra)
	source := "LEAST(" + expires + ", " + accountSourceExpirySQL(extra) + ")"
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
