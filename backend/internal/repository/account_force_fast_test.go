package repository

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountForceFastPersistsWithSchedulerInvalidation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, outboxFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/outboxFails=%t", enabled, outboxFails), func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
				t.Cleanup(func() { _ = client.Close() })
				repo := newAccountRepositoryWithSQL(client, db, nil)
				mock.ExpectBegin()
				mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL")).
					WithArgs(fmt.Sprintf(`{"openai_force_fast":%t}`, enabled), int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
				event := mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
					WithArgs(service.SchedulerOutboxEventAccountChanged, int64(7), nil, nil, sqlmock.AnyArg())
				if outboxFails {
					event.WillReturnError(errors.New("outbox unavailable"))
					mock.ExpectRollback()
				} else {
					event.WillReturnResult(sqlmock.NewResult(1, 1))
					mock.ExpectCommit()
				}
				err = repo.UpdateExtra(context.Background(), 7, map[string]any{service.OpenAIForceFastExtraKey: enabled})
				if outboxFails {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
