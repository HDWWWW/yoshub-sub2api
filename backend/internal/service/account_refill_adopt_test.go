//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type refillAdoptTestRepository struct {
	AccountRepository
	t          *testing.T
	account    Account
	updates    int
	commits    int
	failPolicy bool
}

func (r *refillAdoptTestRepository) GetByID(ctx context.Context, id int64) (*Account, error) {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	require.Equal(r.t, r.account.ID, id)
	a := r.account
	a.Credentials, a.Extra = maps.Clone(a.Credentials), maps.Clone(a.Extra)
	a.GroupIDs = slices.Clone(a.GroupIDs)
	return &a, nil
}
func (r *refillAdoptTestRepository) Update(ctx context.Context, a *Account) error {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	r.account, r.updates = *a, r.updates+1
	return nil
}
func (r *refillAdoptTestRepository) BindGroups(ctx context.Context, _ int64, groups []int64) error {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	r.account.GroupIDs = slices.Clone(groups)
	return nil
}
func (r *refillAdoptTestRepository) UpdateExtra(ctx context.Context, _ int64, extra map[string]any) error {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	if r.failPolicy {
		return errors.New("expiry policy write failed")
	}
	ApplyAccountExpiryExtra(&r.account, extra)
	return nil
}
func (r *refillAdoptTestRepository) SetSchedulable(ctx context.Context, _ int64, value bool) error {
	require.NotNil(r.t, dbent.TxFromContext(ctx))
	r.account.Schedulable = value
	return nil
}
func (r *refillAdoptTestRepository) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	return nil, nil
}
func (r *refillAdoptTestRepository) ScopedAdminCommitted(ctx context.Context, _ int64, _ bool) {
	require.Nil(r.t, dbent.TxFromContext(ctx))
	r.commits++
}

func TestFenduduAdoptionAppliesPolicyWithinTransaction(t *testing.T) {
	for _, mode := range []string{"random", "direct", "wrong_identity", "policy_failure", "commit_failure", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			oldProxy, oldLoad := int64(8), 55
			repo := &refillAdoptTestRepository{t: t, failPolicy: mode == "policy_failure", account: Account{
				ID: 344, Name: "supplier default", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &oldProxy, LoadFactor: &oldLoad,
				Credentials: map[string]any{"email": "fixture@example.test", "access_token": "fresh-access", "refresh_token": "fresh-refresh", "model_mapping": map[string]string{"old": "old"}},
				Extra:       map[string]any{"openai_responses_flatten_namespaces": true},
			}}
			if mode == "wrong_identity" {
				repo.account.Credentials["email"] = "unrelated@example.test"
			}
			if mode == "recovery" {
				repo.account.Extra["refill_job_id"] = "local-job"
			}
			proxy := &scopedSelectionProxyRepo{id: 9}
			admin := &adminServiceImpl{accountRepo: repo, proxyRepo: proxy, groupRepo: &scopedAdminWriteTestGroups{}, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
			mock.ExpectBegin()
			mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`SELECT id FROM accounts`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`SELECT account_id FROM account_groups`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
			switch mode {
			case "wrong_identity", "policy_failure":
				mock.ExpectRollback()
			case "commit_failure":
				mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
			default:
				mock.ExpectCommit()
			}
			lease := time.Now().Add(time.Hour).Unix()
			deadline, extra := ImportedAccountExpiryWithGrace(&lease, map[string]any{"refill_job_id": "local-job", "import_source": "fendudu_api", AccountSourceExpiryExtraKey: lease, AccountSourceExpiryOriginExtraKey: "supplier_api"}, 30)
			rate, autoPause := 1.5, true
			input := &CreateAccountInput{Name: "saved-name", Credentials: map[string]any{"email": "fixture@example.test", "access_token": "stale-access", "refresh_token": "stale-refresh"}, Extra: extra,
				GroupIDs: []int64{2, 3}, Concurrency: 77, Priority: 8, RateMultiplier: &rate, ExpiresAt: deadline, AutoPauseOnExpired: &autoPause, ProxyMode: "random"}
			if mode == "direct" {
				input.ProxyMode = "direct"
			}
			account, err := admin.AdoptRefillAccount(context.Background(), 344, input)
			switch mode {
			case "wrong_identity", "policy_failure", "commit_failure":
				require.Error(t, err)
				require.Zero(t, repo.commits, "failed transactions cannot publish a successful adoption")
			case "recovery":
				require.NoError(t, err)
				require.Zero(t, repo.updates)
				require.Zero(t, proxy.calls, "recovery must not allocate a second proxy")
			default:
				require.NoError(t, err)
				require.Equal(t, 1, repo.commits)
				require.Equal(t, "fresh-access", account.Credentials["access_token"])
				require.Equal(t, "fresh-refresh", account.Credentials["refresh_token"])
				require.NotContains(t, account.Credentials, "model_mapping")
				require.Equal(t, false, account.Extra["openai_responses_flatten_namespaces"])
				require.Equal(t, true, account.Extra[OpenAIPrivacyEnabledExtraKey])
				require.Nil(t, account.LoadFactor)
				require.Equal(t, []int64{2, 3}, account.GroupIDs)
				require.Equal(t, "saved-name", account.Name)
				require.Equal(t, 77, account.Concurrency)
				require.Equal(t, 8, account.Priority)
				require.Equal(t, 1.5, *account.RateMultiplier)
				require.Equal(t, "inactive", account.Status)
				require.False(t, account.Schedulable)
				require.True(t, account.AutoPauseOnExpired)
				require.Equal(t, lease+1800, account.ExpiresAt.Unix())
				if mode == "direct" {
					require.Nil(t, account.ProxyID)
				} else {
					require.EqualValues(t, 9, *account.ProxyID)
					require.Equal(t, 1, proxy.calls)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestFenduduExpiredReceiptPreservesEarlierExpiryAndCredentials(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	earlier := time.Now().Add(-time.Hour).Truncate(time.Second)
	proxyID := int64(8)
	repo := &refillAdoptTestRepository{t: t, account: Account{
		ID: 344, Name: "keep-name", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID, ExpiresAt: &earlier,
		Credentials: map[string]any{"email": "fixture@example.test", "access_token": "fresh-access"},
		Extra:       map[string]any{"existing": "preserved"},
	}}
	proxy := &scopedSelectionProxyRepo{id: 9}
	admin := &adminServiceImpl{accountRepo: repo, proxyRepo: proxy, groupRepo: &scopedAdminWriteTestGroups{}, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SELECT id FROM accounts`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SELECT account_id FROM account_groups`).WithArgs(int64(344)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	lease := earlier.Add(7 * time.Minute).Unix()
	deadline, extra := ImportedAccountExpiryWithGrace(&lease, map[string]any{"refill_job_id": "local-job", "refill_supplier_id": "rvc_fixture", "import_source": "fendudu_api"}, 10)
	input := &CreateAccountInput{Credentials: map[string]any{"email": "fixture@example.test"}, Extra: extra, ExpiresAt: deadline, ProxyMode: "random"}
	require.NoError(t, admin.RecordExpiredRefillAccount(context.Background(), 344, input))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 1, repo.commits)
	require.Equal(t, "local-job", repo.account.Extra["refill_job_id"])
	require.Equal(t, lease, repo.account.Extra[AccountSourceExpiryExtraKey])
	require.Equal(t, "preserved", repo.account.Extra["existing"])
	require.Equal(t, earlier.Unix(), repo.account.ExpiresAt.Unix())
	require.Equal(t, "fresh-access", repo.account.Credentials["access_token"])
	require.Equal(t, "keep-name", repo.account.Name)
	require.Equal(t, "inactive", repo.account.Status)
	require.False(t, repo.account.Schedulable)
	require.EqualValues(t, 8, *repo.account.ProxyID)
	require.Zero(t, proxy.calls)
}
