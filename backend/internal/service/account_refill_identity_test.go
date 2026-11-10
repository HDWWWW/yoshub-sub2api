//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountEditPreservesRefillIdentityFromCurrentAccount(t *testing.T) {
	for _, source := range []string{"fendudu_api", "llmrefund_api"} {
		t.Run(source, func(t *testing.T) {
			repo := &refillAdoptTestRepository{t: t, account: Account{
				ID: 344, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Extra: map[string]any{"refill_job_id": "saved-job", "refill_supplier_id": "saved-order", "refill_plan": "pro20x", "import_source": source},
			}}
			// The edit began before the refill worker adopted the pushed account.
			incoming := map[string]any{"import_source": "codex_session", "same_account_retry_count": 2}
			admin := &adminServiceImpl{accountRepo: &refillEditRepository{repo}}
			updated, err := admin.UpdateAccount(context.Background(), 344, &UpdateAccountInput{Extra: incoming})
			require.NoError(t, err)
			require.Equal(t, "saved-job", updated.Extra["refill_job_id"])
			require.Equal(t, "saved-order", updated.Extra["refill_supplier_id"])
			require.Equal(t, "pro20x", updated.Extra["refill_plan"])
			require.Equal(t, source, updated.Extra["import_source"])
			require.Equal(t, 2, updated.Extra["same_account_retry_count"])
		})
	}
}

type refillEditRepository struct{ *refillAdoptTestRepository }

func (r *refillEditRepository) GetByID(context.Context, int64) (*Account, error) {
	a := r.account
	return &a, nil
}

func (r *refillEditRepository) Update(_ context.Context, a *Account) error {
	r.account = *a
	return nil
}
