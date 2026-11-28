package ovhipv6

import (
	"context"
	"strings"
	"time"
)

type Pool string

const (
	Primary Pool = "primary"
	Backup  Pool = "backup"
)

type poolKey struct{}
type accountEditKey struct{}
type refillRuleKey struct{}

func AccountEdit(ctx context.Context) context.Context {
	return context.WithValue(ctx, accountEditKey{}, true)
}
func IsAccountEdit(ctx context.Context) bool { ok, _ := ctx.Value(accountEditKey{}).(bool); return ok }

// The rule that owns the delivery, independent of other group memberships.
func ForRefillRule(ctx context.Context, groupID int64) context.Context {
	return context.WithValue(ctx, refillRuleKey{}, groupID)
}

func RefillRule(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(refillRuleKey{}).(int64)
	return id, ok && id > 0
}

func WithPool(ctx context.Context, pool Pool) context.Context {
	return context.WithValue(ctx, poolKey{}, pool)
}
func SelectedPool(ctx context.Context) Pool {
	if p, ok := ctx.Value(poolKey{}).(Pool); ok && p == Backup {
		return Backup
	}
	return Primary
}
func ParsePool(s string) (Pool, bool) {
	switch s {
	case "", string(Primary):
		return Primary, true
	case string(Backup):
		return Backup, true
	default:
		return "", false
	}
}

// Prefix backup IDs in persistent work. A pre-upgrade worker cannot mistake a
// backup lease for a primary lease with the same local sequence number.
func StoredID(p Pool, id string) string {
	if p == Backup {
		return "backup:" + id
	}
	return id
}
func SplitID(key string) (Pool, string) {
	if local, ok := strings.CutPrefix(key, "backup:"); ok {
		return Backup, local
	}
	return Primary, key
}

type Retirement struct {
	ID        int64     `json:"id"`
	Address   string    `json:"address"`
	Pool      Pool      `json:"pool"`
	RetiredAt time.Time `json:"retired_at"`
}
