//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestGroupSpeedModeMigrationPreservesLegacyValuesAndAllowsFastFallback(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	// A transaction-local table shadows the real groups table. Exercise the
	// actual old and new SQL without modifying the shared integration fixture.
	_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE groups (
		id BIGINT PRIMARY KEY,
		force_openai_fast BOOLEAN NOT NULL DEFAULT FALSE,
		free_openai_fast BOOLEAN NOT NULL DEFAULT FALSE
	) ON COMMIT DROP`)
	require.NoError(t, err)
	apply := func(name string) {
		t.Helper()
		body, readErr := migrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		_, execErr := tx.ExecContext(ctx, string(body))
		require.NoError(t, execErr, name)
	}
	apply("249_group_force_openai_ultrafast.sql")
	_, err = tx.ExecContext(ctx, `INSERT INTO groups
		(id, force_openai_fast, force_openai_ultrafast, filter_openai_service_tier, free_openai_fast)
		VALUES (1, TRUE, FALSE, FALSE, TRUE), (2, FALSE, TRUE, FALSE, TRUE), (3, FALSE, FALSE, TRUE, FALSE)`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, "SAVEPOINT legacy_constraint")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE groups SET force_openai_ultrafast = TRUE WHERE id = 1")
	require.ErrorContains(t, err, "groups_openai_fast_mode_exclusive")
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT legacy_constraint")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT legacy_constraint")
	require.NoError(t, err)

	apply("251_group_force_openai_ultrafast.sql")
	apply("260_group_openai_speed_modes.sql")
	apply("260_group_openai_speed_modes.sql")
	for _, row := range []struct {
		id                        int64
		fast, ultra, filter, free bool
	}{{1, true, false, false, true}, {2, false, true, false, true}, {3, false, false, true, false}} {
		var fast, ultra, filter, free bool
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT force_openai_fast, force_openai_ultrafast,
			filter_openai_service_tier, free_openai_fast FROM groups WHERE id=$1`, row.id).Scan(&fast, &ultra, &filter, &free))
		require.Equal(t, row.fast, fast)
		require.Equal(t, row.ultra, ultra)
		require.Equal(t, row.filter, filter)
		require.Equal(t, row.free, free)
	}

	for _, flags := range [][3]bool{
		{false, false, false}, {true, false, false}, {false, true, false}, {true, true, false},
		{false, false, true}, {true, false, true}, {false, true, true}, {true, true, true},
	} {
		_, err = tx.ExecContext(ctx, "SAVEPOINT speed_modes")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE groups SET force_openai_fast=$1,
			force_openai_ultrafast=$2, filter_openai_service_tier=$3 WHERE id=1`, flags[0], flags[1], flags[2])
		if flags[2] && (flags[0] || flags[1]) {
			require.ErrorContains(t, err, "groups_openai_fast_mode_exclusive")
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT speed_modes")
			require.NoError(t, err)
		} else {
			require.NoError(t, err)
		}
		_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT speed_modes")
		require.NoError(t, err)
	}
}
