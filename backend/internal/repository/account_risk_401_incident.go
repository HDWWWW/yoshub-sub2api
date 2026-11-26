package repository

import (
	"context"
	"database/sql"
	"time"
)

// Keep the recovery watermark across episodes. A delayed failure from before a
// verified recovery cannot reopen an episode or inflate a newer episode's count.
const opensRisk401Episode = `i.last_401_at <= i.recovered_at AND EXCLUDED.last_401_at > i.recovered_at`
const recordRisk401SQL = `INSERT INTO account_risk_401_incidents AS i (group_id,owner_id,first_event_id,last_401_at,occurrences)
    VALUES ($1,$2,$3,$4,1)
    ON CONFLICT (owner_id,group_id) DO UPDATE SET
        first_event_id = CASE WHEN ` + opensRisk401Episode + ` THEN EXCLUDED.first_event_id ELSE i.first_event_id END,
        occurrences = CASE WHEN ` + opensRisk401Episode + ` THEN 1
            WHEN i.last_401_at > i.recovered_at AND EXCLUDED.last_401_at <= i.recovered_at THEN i.occurrences
            ELSE i.occurrences + 1 END,
        last_401_at = GREATEST(i.last_401_at, EXCLUDED.last_401_at)
    RETURNING first_event_id, occurrences`

func recordRisk401(ctx context.Context, tx *sql.Tx, groupID, ownerID int64, eventID string, at time.Time) (string, int, error) {
	var first string
	var count int
	err := tx.QueryRowContext(ctx, recordRisk401SQL, groupID, ownerID, eventID, at).Scan(&first, &count)
	return first, count, err
}

// A successful check already in flight when the failure occurred is not proof
// of recovery. Healthy accounts have no matching row and incur no writes.
func (s *accountRiskStore) Recover401(ctx context.Context, ownerID int64, startedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_risk_401_incidents SET recovered_at=$2
        WHERE owner_id=$1 AND last_401_at < $2 AND (recovered_at IS NULL OR recovered_at < last_401_at)`, ownerID, startedAt)
	return err
}
