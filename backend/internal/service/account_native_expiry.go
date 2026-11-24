package service

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
)

const (
	AccountNativeExpiryExtraKey   = "yoshub_native_expiry"
	AccountExpiryBaseExtraKey     = "yoshub_expiry_base"
	AccountExpiryManualExtraKey   = "yoshub_expiry_manual"
	AccountDeliveryKeyExtraKey    = "yoshub_delivery_key"
	AccountDeliveryExpiryExtraKey = "yoshub_delivery_expires_at"
)

// IsAccountExpiryExtra identifies metadata owned by the expiry policy. Ordinary
// extra replacements must preserve it from the locked row, not a stale form.
func IsAccountExpiryExtra(key string) bool {
	switch key {
	case AccountNativeExpiryExtraKey, AccountExpiryBaseExtraKey, AccountExpiryManualExtraKey,
		AccountDeliveryKeyExtraKey, AccountDeliveryExpiryExtraKey, AccountImportExpiryGraceExtraKey,
		AccountSourceExpiryExtraKey, AccountSourceExpiryOriginExtraKey, AccountSourceExpiryCheckedExtraKey,
		"import_original_expires_at", "import_expiry_offset_seconds":
		return true
	}
	return false
}

func expiryTimestamp(value any) int64 {
	raw := fmt.Sprint(value)
	if n, ok := value.(float64); ok {
		raw = strconv.FormatFloat(n, 'f', -1, 64)
	}
	if len(raw) == 0 || len(raw) > 12 {
		return 0
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n > 253402214399 {
		return 0
	}
	return n
}

func (a *Account) HasNativeExpiry() bool {
	return a != nil && a.IsOpenAIOAuth() && a.Extra[AccountNativeExpiryExtraKey] == true
}

func (a *Account) hasExpiryPolicy() bool {
	if a == nil || !a.IsOpenAIOAuth() {
		return false
	}
	_, current := a.Extra[AccountImportExpiryGraceExtraKey]
	_, legacy := a.Extra["import_expiry_offset_seconds"]
	return current || legacy || a.HasNativeExpiry()
}

// MaterializeAccountExpiry converts the old internal extension without changing
// its effective deadline. It is also used for explicit retention settings.
func MaterializeAccountExpiry(a *Account) {
	if a == nil || !a.IsOpenAIOAuth() || a.HasNativeExpiry() {
		return
	}
	extra := maps.Clone(a.Extra)
	if extra == nil {
		extra = make(map[string]any)
	}
	base := a.ExpiresAt
	deadline := a.SchedulingExpiresAt()
	extra[AccountExpiryBaseExtraKey] = int64(0)
	if base != nil {
		extra[AccountExpiryBaseExtraKey] = base.Unix()
	}
	extra[AccountImportExpiryGraceExtraKey] = a.ExpiryGraceSeconds()
	extra[AccountNativeExpiryExtraKey] = true
	extra[AccountExpiryManualExtraKey] = false
	a.Extra, a.ExpiresAt = extra, deadline
}

func recalculateNativeExpiry(a *Account) {
	base := expiryTimestamp(a.Extra[AccountExpiryBaseExtraKey])
	if base == 0 {
		a.ExpiresAt = nil
		return
	}
	if source, origin := a.SourceExpiresAt(); origin != "local" && origin != "supplier_pending" && source != nil && source.Unix() < base {
		base = source.Unix()
	}
	deadline := time.Unix(base+int64(a.ExpiryGraceSeconds()), 0)
	a.ExpiresAt = &deadline
}

// ApplyAccountExpiryExtra runs under a row lock in persistence. Source syncs
// respect a manually chosen deadline; saving retention explicitly recalculates it.
func ApplyAccountExpiryExtra(a *Account, updates map[string]any) {
	_, retention := updates[AccountImportExpiryGraceExtraKey]
	if retention {
		MaterializeAccountExpiry(a)
	}
	a.Extra = maps.Clone(a.Extra)
	if a.Extra == nil {
		a.Extra = make(map[string]any)
	}
	maps.Copy(a.Extra, updates)
	if !a.HasNativeExpiry() {
		return
	}
	if retention {
		a.Extra[AccountExpiryManualExtraKey] = false
	}
	if a.Extra[AccountExpiryManualExtraKey] != true {
		recalculateNativeExpiry(a)
	}
}

type accountExpiryWriteKey struct{}

func WithAccountExpiryWrite(ctx context.Context, expires *int64) context.Context {
	return context.WithValue(ctx, accountExpiryWriteKey{}, expires)
}

// ReapplyAccountExpiryWrite prevents credential refreshes and stale edits from
// undoing a concurrent retention change. Called after loading the locked row.
func ReapplyAccountExpiryWrite(ctx context.Context, a *Account) {
	if expires, _ := ctx.Value(accountExpiryWriteKey{}).(*int64); expires != nil {
		ApplyAccountExpiryWrite(ctx, a, *expires)
	}
}

// MergeLockedAccountExpiry protects the policy and deadline from unrelated
// full-object writes (including token refreshes) based on an older snapshot.
func MergeLockedAccountExpiry(ctx context.Context, incoming, current *Account) {
	if incoming.Extra == nil {
		incoming.Extra = make(map[string]any)
	}
	for key := range incoming.Extra {
		if IsAccountExpiryExtra(key) {
			delete(incoming.Extra, key)
		}
	}
	for key, value := range current.Extra {
		if IsAccountExpiryExtra(key) {
			incoming.Extra[key] = value
		}
	}
	if current.hasExpiryPolicy() {
		incoming.ExpiresAt = current.ExpiresAt
	}
	ReapplyAccountExpiryWrite(ctx, incoming)
}

func accountDeliveryKeyMatches(ctx context.Context, a *Account) bool {
	key := adminaccess.KeyIdentity(ctx)
	return a != nil && a.HasNativeExpiry() && key != "" && a.Extra[AccountDeliveryKeyExtraKey] == key
}

func ApplyAccountExpiryWrite(ctx context.Context, a *Account, expires int64) {
	if a.hasExpiryPolicy() {
		MaterializeAccountExpiry(a)
	}
	a.Extra = maps.Clone(a.Extra)
	if a.Extra == nil {
		a.Extra = make(map[string]any)
	}
	if accountDeliveryKeyMatches(ctx, a) {
		if expires == expiryTimestamp(a.Extra[AccountDeliveryExpiryExtraKey]) {
			return // An echoed supplier timestamp must not undo an admin edit.
		}
		a.Extra[AccountDeliveryExpiryExtraKey] = expires
		a.Extra[AccountExpiryBaseExtraKey] = expires
		if a.Extra[AccountSourceExpiryOriginExtraKey] != "supplier_api" && a.Extra[AccountSourceExpiryOriginExtraKey] != "supplier_pending" {
			a.Extra[AccountSourceExpiryExtraKey] = expires
			a.Extra[AccountSourceExpiryOriginExtraKey] = "import"
		}
		if a.Extra[AccountExpiryManualExtraKey] != true {
			recalculateNativeExpiry(a)
		}
		return
	}
	current := int64(0)
	if a.ExpiresAt != nil {
		current = a.ExpiresAt.Unix()
	}
	if expires == current {
		return
	}
	if a.HasNativeExpiry() {
		a.Extra[AccountExpiryManualExtraKey] = true
	}
	a.ExpiresAt = nil
	if expires > 0 {
		deadline := time.Unix(expires, 0)
		a.ExpiresAt = &deadline
	}
}

// AccountForAdminRead changes only the importing key's delivery view. The stored
// account, scheduler, JWT administrator, and other keys use the native deadline.
func AccountForAdminRead(ctx context.Context, a *Account) *Account {
	if !accountDeliveryKeyMatches(ctx, a) {
		return a
	}
	copy := *a
	copy.ExpiresAt = nil
	if original := expiryTimestamp(a.Extra[AccountDeliveryExpiryExtraKey]); original > 0 {
		deadline := time.Unix(original, 0)
		copy.ExpiresAt = &deadline
	}
	return &copy
}
