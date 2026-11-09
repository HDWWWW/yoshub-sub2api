//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestModerationAccountBackfillAndFullEvidencePostgres(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	query := func(dest any, sql string, args ...any) {
		rows, err := tx.QueryContext(ctx, sql, args...)
		require.NoError(t, err)
		defer rows.Close()
		require.True(t, rows.Next())
		require.NoError(t, rows.Scan(dest))
	}
	client := tx.Client()
	user := mustCreateUser(t, client, &service.User{Email: "policy-account@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-policy-migration", Name: "gateway"})
	account := mustCreateAccount(t, client, &service.Account{Name: "pool-returning-account"})
	wrong := mustCreateAccount(t, client, &service.Account{Name: "different-pool-account"})
	for _, item := range []struct{ request, action string }{{"policy-ops", "cyber_policy"}, {"policy-usage", "bio_policy"}, {"keyword", "keyword_block"}, {"wrong-key", "cyber_policy"}} {
		_, err := tx.ExecContext(ctx, `INSERT INTO content_moderation_logs(request_id,api_key_id,action) VALUES($1,$2,$3)`, item.request, key.ID, item.action)
		require.NoError(t, err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO ops_error_logs(request_id,api_key_id,account_id,error_phase,error_type) VALUES
		('policy-ops',$1,$2,'request','cyber_policy'),
		('keyword',$1,$2,'request','keyword_block'),
		('wrong-key',$3,$2,'request','cyber_policy')`, key.ID, account.ID, key.ID+1)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,request_type) VALUES
		($1,$2,$3,'policy-usage','gpt-test',4),($1,$2,$4,'policy-ops','gpt-test',4)`, user.ID, key.ID, account.ID, wrong.ID)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("259_content_moderation_account.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(migration))
		require.NoError(t, err)
		var count int
		query(&count, `SELECT COUNT(*) FROM content_moderation_logs
			WHERE request_id IN ('policy-ops','policy-usage') AND account_id=$1 AND account_name=$2`, account.ID, account.Name)
		require.Equal(t, 2, count)
		query(&count, `SELECT COUNT(*) FROM content_moderation_logs
			WHERE request_id IN ('keyword','wrong-key') AND account_id IS NULL`)
		require.Equal(t, 2, count)
	}
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET name='renamed',deleted_at=NOW() WHERE id=$1`, account.ID)
	require.NoError(t, err)
	var name string
	query(&name, `SELECT account_name FROM content_moderation_logs WHERE request_id='policy-ops'`)
	require.Equal(t, account.Name, name)

	require.NoError(t, tx.Rollback()) // Release migration locks before repository reads.

	// Verify the real repository stores and reads an entire large conversation.
	repo := NewContentModerationRepository(integrationDB)
	accountID := int64(363)
	full := &service.ContentModerationLog{Action: "bio_policy", AccountID: &accountID, AccountName: "retained-pool-name", InputEvidence: &service.ContentModerationInputEvidence{
		ContentScope: "full_conversation", Text: strings.Repeat("瀹屾暣娑堟伅\n", 40000) + "FINAL-EVIDENCE",
		Policy: &service.ContentModerationPolicyEvidence{Code: "bio_policy", UpstreamBody: "complete upstream error"},
	}}
	require.NoError(t, repo.CreateLog(ctx, full))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM content_moderation_logs WHERE id=$1", full.ID)
	})
	stored, err := repo.GetLogInput(ctx, full.ID)
	require.NoError(t, err)
	require.Equal(t, full.InputEvidence, stored.Evidence)
	logs, _, err := repo.ListLogs(ctx, service.ContentModerationLogFilter{Search: "retained-pool-name"})
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.Equal(t, accountID, *logs[0].AccountID)
	require.Nil(t, logs[0].InputEvidence)
}
