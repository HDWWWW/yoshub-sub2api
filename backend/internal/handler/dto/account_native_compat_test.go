package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeAccountResponseHidesImportPolicyAndPreservesSupplierFields(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	a := &service.Account{ID: 7, Priority: 17, ExpiresAt: &expiry, Schedulable: false, GroupIDs: []int64{2, 5},
		Extra: map[string]any{"supplier": "unchanged", service.AccountImportExpiryGraceExtraKey: 3600, service.AccountManualPriorityExtraKey: 0,
			service.AccountNativeExpiryExtraKey: true, service.AccountExpiryBaseExtraKey: expiry.Unix() - 3600, service.AccountExpiryManualExtraKey: false,
			service.AccountDeliveryKeyExtraKey: "legacy", service.AccountDeliveryExpiryExtraKey: expiry.Unix() - 3600,
			service.AccountSourceExpiryExtraKey: expiry.Unix(), service.AccountSourceExpiryOriginExtraKey: "supplier_api", service.AccountSourceExpiryCheckedExtraKey: "2026-11-01T00:00:00Z",
			"import_original_expires_at": expiry.Unix(), "import_expiry_offset_seconds": 3600}}
	dto := AccountFromService(a)
	require.Equal(t, 17, dto.Priority)
	require.NotNil(t, dto.ExpiresAt)
	require.Equal(t, expiry.Unix(), *dto.ExpiresAt)
	require.False(t, dto.Schedulable)
	require.Equal(t, []int64{2, 5}, dto.GroupIDs)
	require.Equal(t, map[string]any{"supplier": "unchanged"}, dto.Extra)
	require.Equal(t, dto.Extra, AccountFromServiceShallow(a).Extra)
	require.Len(t, a.Extra, 13, "redaction must not modify the source/cached metadata")
}
