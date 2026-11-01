//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type nativeIPv6Allocator struct {
	ProxyRepository
	calls int
}

func (r *nativeIPv6Allocator) SelectAccountProxy(ctx context.Context, mode string) (int64, error) {
	if dbent.TxFromContext(ctx) == nil || mode != "ovh_ipv6" {
		return 0, fmt.Errorf("wrong allocation scope")
	}
	r.calls++
	return int64(700 + r.calls), nil
}

type nativeIPv6AccountRepo struct {
	*scopedSelectionAccountRepo
	failID int64
}

func (r *nativeIPv6AccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	a, err := r.scopedSelectionAccountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := r.login.sql(ctx).QueryContext(ctx, `SELECT parent_account_id FROM accounts WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		err = rows.Scan(&a.ParentAccountID)
	}
	if err != nil {
		return nil, err
	}
	return a, rows.Err()
}

func (r *nativeIPv6AccountRepo) CreateWithAccountGroups(ctx context.Context, a *Account, groups []AccountGroup) error {
	if dbent.TxFromContext(ctx) == nil {
		return fmt.Errorf("duplicate allocation escaped its transaction")
	}
	if err := r.Create(ctx, a); err != nil {
		return err
	}
	ids := make([]int64, len(groups))
	for i, g := range groups {
		ids[i] = g.GroupID
	}
	return r.BindGroups(ctx, a.ID, ids)
}

func (r *nativeIPv6AccountRepo) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	out := []*Account{}
	for _, id := range ids {
		a, e := r.GetByID(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, nil
}
func (r *nativeIPv6AccountRepo) BulkUpdate(ctx context.Context, ids []int64, u AccountBulkUpdate) (int64, error) {
	if dbent.TxFromContext(ctx) == nil {
		return 0, fmt.Errorf("bulk allocation escaped its transaction")
	}
	for _, id := range ids {
		_, e := r.login.sql(ctx).ExecContext(ctx, `UPDATE accounts SET proxy_id=$2,name=COALESCE($3,name) WHERE id=$1`, id, u.ProxyID, u.Name)
		if e != nil {
			return 0, e
		}
		if id == r.failID {
			return 0, fmt.Errorf("failed after account write")
		}
	}
	return int64(len(ids)), nil
}

func TestNativeIPv6CreateEditAndBulkUseAtomicAccountTransactions(t *testing.T) {
	login, db := scopedAdminAccountTestDB(t)
	_, err := db.Exec(`CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT); INSERT INTO accounts(id,name,platform,type,proxy_id) VALUES(1001,'first','openai','apikey',11),(1002,'second','openai','apikey',12)`)
	require.NoError(t, err)
	base := &scopedAdminWriteTestRepo{login: login}
	r := &nativeIPv6AccountRepo{scopedSelectionAccountRepo: &scopedSelectionAccountRepo{base}}
	allocator := &nativeIPv6Allocator{}
	s := &adminServiceImpl{accountRepo: r, proxyRepo: allocator, groupRepo: &scopedAdminWriteTestGroups{}, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
	ctx := context.Background()
	created, err := s.CreateAccount(ctx, &CreateAccountInput{Name: "native", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}, ProxyMode: "ovh_ipv6", SkipDefaultGroupBind: true})
	require.NoError(t, err)
	require.EqualValues(t, 701, *created.ProxyID)
	require.Equal(t, 1, allocator.calls)
	updated, err := s.UpdateAccount(ctx, 1001, &UpdateAccountInput{ProxyMode: "ovh_ipv6"})
	require.NoError(t, err)
	require.EqualValues(t, 702, *updated.ProxyID)
	base.failBind = true
	groups := []int64{1}
	_, err = s.UpdateAccount(ctx, 1001, &UpdateAccountInput{Name: "rolled back", ProxyMode: "ovh_ipv6", GroupIDs: &groups})
	require.Error(t, err)
	base.failBind = false
	stored, err := r.GetByID(ctx, 1001)
	require.NoError(t, err)
	require.EqualValues(t, 702, *stored.ProxyID)
	require.Equal(t, "first", stored.Name)
	r.failID = 1002
	result, err := s.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{1001, 1002, 1001}, ProxyMode: "ovh_ipv6", Name: "bulk"})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, result.Failed)
	stored, err = r.GetByID(ctx, 1001)
	require.NoError(t, err)
	require.EqualValues(t, 704, *stored.ProxyID)
	require.Equal(t, "bulk", stored.Name)
	stored, err = r.GetByID(ctx, 1002)
	require.NoError(t, err)
	require.EqualValues(t, 12, *stored.ProxyID)
	require.Equal(t, "second", stored.Name)
	require.Equal(t, 5, allocator.calls, "failed attempts are consumed; duplicate IDs are not allocated twice")
}

type nativeIPv6ManagedAllocator struct{ nativeIPv6Allocator }

func (*nativeIPv6ManagedAllocator) ManagedIPv6Addresses(context.Context, []int64) (map[int64]string, error) {
	return map[int64]string{11: "2001:db8::b"}, nil
}

func TestNativeIPv6DuplicateGetsFreshAddressAndRollsBackFailedGroups(t *testing.T) {
	login, db := scopedAdminAccountTestDB(t)
	_, err := db.Exec(`CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT); INSERT INTO accounts(id,name,platform,type,proxy_id) VALUES(1001,'source','openai','apikey',11); INSERT INTO account_groups VALUES(1001,1)`)
	require.NoError(t, err)
	base := &scopedAdminWriteTestRepo{login: login}
	r := &nativeIPv6AccountRepo{scopedSelectionAccountRepo: &scopedSelectionAccountRepo{base}}
	allocator := &nativeIPv6ManagedAllocator{}
	s := &adminServiceImpl{accountRepo: r, accountDuplicateRepo: r, proxyRepo: allocator, groupRepo: &scopedAdminWriteTestGroups{}, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
	ctx := context.Background()
	duplicate, err := s.DuplicateAccount(ctx, 1001, "admin", "")
	require.NoError(t, err)
	require.EqualValues(t, 701, *duplicate.ProxyID)
	require.False(t, duplicate.Schedulable)
	require.Equal(t, []int64{1}, duplicate.GroupIDs)
	stored, err := r.GetByID(ctx, duplicate.ID)
	require.NoError(t, err)
	require.EqualValues(t, 701, *stored.ProxyID)
	require.Equal(t, []int64{1}, stored.GroupIDs)
	base.failBind = true
	_, err = s.DuplicateAccount(ctx, 1001, "admin", "")
	require.Error(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count))
	require.Equal(t, 2, count, "failed duplicate must roll back its account write")
	stored, err = r.GetByID(ctx, 1001)
	require.NoError(t, err)
	require.EqualValues(t, 11, *stored.ProxyID, "copying must not retire the source address")
	require.Equal(t, 2, allocator.calls)
}

func TestNativeIPv6ShadowLegacyEditPreservesInheritedProxy(t *testing.T) {
	login, db := scopedAdminAccountTestDB(t)
	_, err := db.Exec(`CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT); INSERT INTO accounts(id,name,platform,type,proxy_id,parent_account_id) VALUES(1002,'shadow','openai','oauth',11,1001)`)
	require.NoError(t, err)
	r := &nativeIPv6AccountRepo{scopedSelectionAccountRepo: &scopedSelectionAccountRepo{&scopedAdminWriteTestRepo{login: login}}}
	allocator := &nativeIPv6Allocator{}
	s := &adminServiceImpl{accountRepo: r, proxyRepo: allocator, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
	ctx := context.Background()
	wrongProxy := int64(42)
	updated, err := s.UpdateAccount(ctx, 1002, &UpdateAccountInput{Name: "renamed", ProxyID: &wrongProxy})
	require.NoError(t, err)
	require.EqualValues(t, 11, *updated.ProxyID)
	require.Equal(t, "renamed", updated.Name)
	_, err = s.UpdateAccount(ctx, 1002, &UpdateAccountInput{ProxyMode: "ovh_ipv6"})
	require.ErrorContains(t, err, "影子账号")
	require.Zero(t, allocator.calls)
}
