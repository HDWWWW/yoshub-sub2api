package service

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// AccountImportExpiryGraceExtraKey is private scheduling metadata. Native admin
// reads/writes and exports retain the supplier's expires_at and priority values.
const AccountImportExpiryGraceExtraKey = "yoshub_import_expiry_grace_seconds"
const AccountSourceExpiryExtraKey = "yoshub_source_expires_at"
const AccountSourceExpiryOriginExtraKey = "yoshub_source_expiry_origin"
const AccountSourceExpiryCheckedExtraKey = "yoshub_source_expiry_checked_at"

func isAccountImportExpiryExtra(key string) bool {
	return key == AccountManualPriorityExtraKey || key == AccountImportExpiryGraceExtraKey || key == AccountSourceExpiryExtraKey || key == AccountSourceExpiryOriginExtraKey || key == AccountSourceExpiryCheckedExtraKey || key == "import_original_expires_at" || key == "import_expiry_offset_seconds"
}

func (a *Account) SourceExpiresAt() (*time.Time, string) {
	if a != nil && a.IsOpenAIOAuth() {
		for _, key := range []string{AccountSourceExpiryExtraKey, "import_original_expires_at"} {
			raw := fmt.Sprint(a.Extra[key])
			if numeric, ok := a.Extra[key].(float64); ok {
				raw = strconv.FormatFloat(numeric, 'f', -1, 64)
			}
			valid := len(raw) > 0 && len(raw) <= 12
			for _, c := range raw {
				if c < '0' || c > '9' {
					valid = false
				}
			}
			seconds, err := strconv.ParseInt(raw, 10, 64)
			if valid && err == nil && seconds > 0 && seconds <= 253402214399 {
				t := time.Unix(seconds, 0).UTC()
				origin := "import"
				if key == AccountSourceExpiryExtraKey && a.Extra[AccountSourceExpiryOriginExtraKey] == "supplier_api" {
					origin = "supplier_api"
				}
				return &t, origin
			}
		}
	}
	if a == nil {
		return nil, "unknown"
	}
	return a.ExpiresAt, "local"
}

// NativeAccountExtra removes only the import extension's private metadata.
func NativeAccountExtra(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	out := make(map[string]any, len(extra))
	for key, value := range extra {
		if !isAccountImportExpiryExtra(key) {
			out[key] = value
		}
	}
	return out
}

func preserveAccountImportExpiryExtra(current, incoming map[string]any) map[string]any {
	out := NativeAccountExtra(incoming)
	for key, value := range current {
		if isAccountImportExpiryExtra(key) {
			if out == nil {
				out = make(map[string]any)
			}
			out[key] = value
		}
	}
	return out
}

// SchedulingExpiresAt adds a bounded, import-only grace period without changing
// ExpiresAt or OAuth token expiry. Keep validation aligned with the SQL predicate.
func (a *Account) SchedulingExpiresAt() *time.Time {
	if a == nil || a.ExpiresAt == nil {
		return nil
	}
	if !a.IsOpenAIOAuth() {
		return a.ExpiresAt
	}
	base := a.ExpiresAt
	if lease, origin := a.SourceExpiresAt(); origin != "local" && lease != nil && lease.Before(*base) {
		base = lease
	}
	deadline := base.Add(time.Duration(a.ExpiryGraceSeconds()) * time.Second)
	return &deadline
}

func (a *Account) ExpiryGraceSeconds() int {
	if a == nil || !a.IsOpenAIOAuth() {
		return 0
	}
	value, hasGrace := a.Extra[AccountImportExpiryGraceExtraKey]
	if !hasGrace {
		value = a.Extra["import_expiry_offset_seconds"]
	}
	raw := fmt.Sprint(value)
	if len(raw) == 0 || len(raw) > 5 {
		return 0
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0
		}
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 || seconds > 86400 {
		return 0
	}
	return seconds
}

func (a *Account) isSchedulingExpired(now time.Time) bool {
	deadline := a.SchedulingExpiresAt()
	return a.AutoPauseOnExpired && deadline != nil && !now.Before(*deadline)
}

// SchedulingPriority belongs only to a request's candidate copy, never to the
// persisted Account.Priority or a shared scheduler snapshot.
func (a *Account) SchedulingPriority() int {
	if a == nil {
		return 0
	}
	if priority, manual := a.ManualSchedulingPriority(); manual {
		return priority
	}
	if a.schedulingPriority > 0 {
		return a.schedulingPriority
	}
	return a.Priority
}

func accountPointersWithExpiryPriority(accounts []*Account, cfg *config.Config, now time.Time) []*Account {
	if cfg == nil || !cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled {
		return accounts
	}
	ranked := accountsWithExpiryPriority(derefAccounts(accounts), cfg, now)
	out := make([]*Account, len(ranked))
	for i := range ranked {
		out[i] = &ranked[i]
	}
	return out
}

func accountsWithExpiryPriority(accounts []Account, cfg *config.Config, now time.Time) []Account {
	if len(accounts) == 0 || cfg == nil || !cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled {
		return accounts
	}
	// Copy before assigning the derived rank, including when a repository returns
	// shared backing storage. Maps and native fields are never modified here.
	out := append([]Account(nil), accounts...)
	type entry struct {
		index    int
		class    int
		expiry   *time.Time
		priority int
	}
	order := make([]entry, len(out))
	for i := range out {
		expiry := out[i].SchedulingExpiresAt()
		class := 1
		if expiry != nil {
			class = 0
			if !expiry.After(now) {
				class = 2
			}
		}
		order[i] = entry{index: i, class: class, expiry: expiry, priority: out[i].Priority}
	}
	compare := func(a, b entry) int {
		if a.class != b.class {
			return a.class - b.class
		}
		if a.class == 0 {
			return a.expiry.Compare(*b.expiry)
		}
		if a.class == 1 {
			if a.priority < b.priority {
				return -1
			}
			if a.priority > b.priority {
				return 1
			}
		}
		return 0
	}
	sort.SliceStable(order, func(i, j int) bool { return compare(order[i], order[j]) < 0 })
	rank := 1
	for i, item := range order {
		if i > 0 && compare(order[i-1], item) != 0 {
			rank++
		}
		out[item.index].schedulingPriority = rank
	}
	return out
}
