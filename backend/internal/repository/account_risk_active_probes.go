package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Keep per-account ownership outside the document that draining legacy workers
// rewrite. Reservation changes share the existing advisory transaction lock.
const accountRiskActiveProbesSettingKey = "account_risk_active_probes_v1"

func readAccountRiskActiveProbes(ctx context.Context, query func(context.Context, string, ...any) *sql.Row, state *service.AccountRiskState) error {
	var raw string
	err := query(ctx, "SELECT value FROM settings WHERE key=$1", accountRiskActiveProbesSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		state.ActiveProbes = nil
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), &state.ActiveProbes); err != nil {
		return err
	}
	for id, pending := range state.ActiveProbes {
		if pending == nil || id <= 0 || pending.AccountID != id || pending.GroupID <= 0 || pending.ID == "" || pending.ExpiresAt.IsZero() {
			return errors.New("invalid account risk probe reservation")
		}
	}
	return nil
}

func syncAccountRiskLegacyGuard(state *service.AccountRiskState) {
	if state.Pending != nil && state.Pending.Kind != service.AccountRiskConcurrentGuard {
		return // A real old-version probe owns this reservation until completion.
	}
	state.Pending = nil
	var expires time.Time
	for _, pending := range state.ActiveProbes {
		if pending.ExpiresAt.After(expires) {
			expires = pending.ExpiresAt
		}
	}
	if !expires.IsZero() {
		state.Pending = &service.AccountRiskPending{ID: service.AccountRiskConcurrentGuard,
			Kind: service.AccountRiskConcurrentGuard, ExpiresAt: expires}
	}
}
