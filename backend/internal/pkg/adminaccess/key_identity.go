package adminaccess

import "context"

type legacyKeyContextKey struct{}

// WithLegacyKey identifies the authenticated logical key, never its secret.
// It does not grant permissions or create a scoped principal.
func WithLegacyKey(ctx context.Context) context.Context {
	return context.WithValue(ctx, legacyKeyContextKey{}, true)
}

func KeyIdentity(ctx context.Context) string {
	if principal := FromContext(ctx); principal != nil {
		return "scoped:" + principal.KeyID
	}
	if legacy, _ := ctx.Value(legacyKeyContextKey{}).(bool); legacy {
		return "legacy"
	}
	return ""
}
