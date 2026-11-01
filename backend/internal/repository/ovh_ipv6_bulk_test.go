//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOVHIPv6BulkRealRepositoryAllocatesAndRotates(t *testing.T) {
	db, proxies, _ := ovhRepoFixtureWithAccountSchema(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := proxies.client
	_, err := db.Exec(`CREATE TABLE scheduler_outbox(id BIGSERIAL PRIMARY KEY,event_type TEXT NOT NULL,account_id BIGINT,group_id BIGINT,payload JSONB,dedup_key TEXT); CREATE UNIQUE INDEX scheduler_outbox_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL`)
	require.NoError(t, err)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	admin := service.NewAdminService(nil, nil, NewAdminGroupRepository(client, db), accounts, proxies, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	ids := []int64{}
	for _, name := range []string{"first", "second"} {
		a, err := client.Account.Create().SetName(name).SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeOAuth).SetCredentials(map[string]any{"access_token": "fixture"}).SetExtra(map[string]any{}).Save(ctx)
		require.NoError(t, err)
		ids = append(ids, a.ID)
	}
	previousProxyIDs := map[int64]int64{}
	for pass := range 3 {
		input := &service.BulkUpdateAccountsInput{AccountIDs: ids, ProxyMode: "ovh_ipv6"}
		if pass == 1 {
			input.AccountIDs = nil
			input.Filters = &service.BulkUpdateAccountFilters{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
		}
		var shadowID int64
		if pass == 2 {
			shadow, err := client.Account.Create().SetName("shadow").SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeOAuth).SetCredentials(map[string]any{}).SetExtra(map[string]any{}).SetParentAccountID(ids[0]).SetQuotaDimension("spark").SetProxyID(previousProxyIDs[ids[0]]).Save(ctx)
			require.NoError(t, err)
			shadowID = shadow.ID
		}
		result, err := admin.BulkUpdateAccounts(ctx, input)
		require.NoError(t, err)
		require.Zero(t, result.Failed, "pass=%d results=%+v", pass, result.Results)
		require.Equal(t, 2, result.Success)
		bound := map[int64]bool{}
		for _, id := range ids {
			a, err := accounts.GetByID(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, a.ProxyID)
			require.NotNil(t, a.Proxy)
			require.False(t, bound[*a.ProxyID])
			bound[*a.ProxyID] = true
			if previousProxyID := previousProxyIDs[id]; previousProxyID != 0 {
				require.NotEqual(t, previousProxyID, *a.ProxyID)
				var retired bool
				require.NoError(t, db.QueryRow(`SELECT l.state='retiring' AND p.deleted_at IS NOT NULL FROM yoshub_ovh_ipv6_leases l JOIN proxies p ON p.id=l.proxy_id WHERE p.id=$1`, previousProxyID).Scan(&retired))
				require.True(t, retired)
			}
			previousProxyIDs[id] = *a.ProxyID
			var owner int64
			var state string
			require.NoError(t, db.QueryRow(`SELECT owner_account_id,state FROM yoshub_ovh_ipv6_leases WHERE proxy_id=$1`, *a.ProxyID).Scan(&owner, &state))
			require.Equal(t, id, owner)
			require.Equal(t, "bound", state)
			var actualAddress string
			require.NoError(t, db.QueryRow(`SELECT host(address) FROM yoshub_ovh_ipv6_leases WHERE proxy_id=$1`, *a.ProxyID).Scan(&actualAddress))
			for _, entry := range result.Results {
				if entry.AccountID == id {
					require.Equal(t, *a.ProxyID, entry.ProxyID)
					require.Equal(t, actualAddress, entry.ManagedIPv6)
				}
			}
		}
		if shadowID != 0 {
			shadow, err := accounts.GetByID(ctx, shadowID)
			require.NoError(t, err)
			require.Equal(t, previousProxyIDs[ids[0]], *shadow.ProxyID)
		}
	}
	_, err = db.Exec(`CREATE FUNCTION fail_one_ipv6_update() RETURNS TRIGGER AS $$ BEGIN IF NEW.name='first' THEN RAISE EXCEPTION 'fixture rejects one account'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE TRIGGER fail_one_ipv6_update BEFORE UPDATE OF proxy_id ON accounts FOR EACH ROW EXECUTE FUNCTION fail_one_ipv6_update()`)
	require.NoError(t, err)
	result, err := admin.BulkUpdateAccounts(ctx, &service.BulkUpdateAccountsInput{AccountIDs: ids, ProxyMode: "ovh_ipv6"})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, result.Failed)
	first, err := accounts.GetByID(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, previousProxyIDs[ids[0]], *first.ProxyID)
	for _, entry := range result.Results {
		if entry.AccountID == ids[0] {
			require.False(t, entry.Success)
			require.Contains(t, entry.Error, "fixture rejects one account")
			require.Zero(t, entry.ProxyID)
			require.Empty(t, entry.ManagedIPv6)
		} else {
			require.True(t, entry.Success)
			require.NotZero(t, entry.ProxyID)
			require.NotEmpty(t, entry.ManagedIPv6)
		}
	}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "password")
	require.NotContains(t, string(encoded), "test-only-password")
}
