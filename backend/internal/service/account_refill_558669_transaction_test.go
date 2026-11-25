//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type refill558669TransactionRepo struct {
	*refillAdoptTestRepository
	writes int
}

func (r *refill558669TransactionRepo) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	require.Equal(r.t, r.account.ID, id)
	r.account.Credentials = maps.Clone(credentials)
	r.writes++
	return nil
}

func TestRefill558669CredentialVersionCommitsAtomically(t *testing.T) {
	for _, scenario := range []string{"success", "already_applied", "wrong_identity", "marker_failure", "commit_failure"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &refill558669TransactionRepo{refillAdoptTestRepository: &refillAdoptTestRepository{t: t, failPolicy: scenario == "marker_failure", account: Account{
				ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: "inactive", Concurrency: 77,
				Credentials: map[string]any{"email": "fixture@example.test", "access_token": "old", "model_mapping": map[string]string{"old": "keep"}},
				Extra:       map[string]any{"refill_job_id": "local", "refill_supplier_id": "task", "import_source": "558669_api", "refill_delivery_version": int64(1)},
			}}}
			if scenario == "already_applied" {
				repo.account.Extra["refill_delivery_version"] = int64(2)
			}
			if scenario == "wrong_identity" {
				repo.account.Extra["refill_supplier_id"] = "other"
			}
			admin := &adminServiceImpl{accountRepo: repo, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
			mock.ExpectBegin()
			mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`SELECT id FROM accounts`).WithArgs(int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`SELECT account_id FROM account_groups`).WithArgs(int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
			switch scenario {
			case "wrong_identity", "marker_failure":
				mock.ExpectRollback()
			case "commit_failure":
				mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
			default:
				mock.ExpectCommit()
			}
			provider := &refillTestProvider{}
			d, err := provider.Delivery(context.Background(), "fixture", "task")
			require.NoError(t, err)
			d.Accounts[0].Credentials["email"] = "fixture@example.test"
			j := AccountRefillJob{ID: "local", SupplierID: "task", Provider: "558669", Status: "imported", AccountID: 42, DeliveryVersion: 2}
			err = admin.Sync558669Delivery(context.Background(), j, d)
			if scenario == "success" || scenario == "already_applied" {
				require.NoError(t, err)
				require.Equal(t, 1, repo.commits)
				require.Equal(t, int64(2), refill558669Version(repo.account.Extra))
			} else {
				require.Error(t, err)
				require.Zero(t, repo.commits)
			}
			if scenario == "already_applied" || scenario == "wrong_identity" {
				require.Zero(t, repo.writes)
			}
			require.Equal(t, "inactive", repo.account.Status)
			require.Equal(t, 77, repo.account.Concurrency)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
