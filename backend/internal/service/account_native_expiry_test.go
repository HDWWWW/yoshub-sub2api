//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/stretchr/testify/require"
)

func nativeExpiryFixture() *Account {
	original := time.Now().Add(-15 * time.Minute).Truncate(time.Second).Unix()
	deadline, extra := ImportedAccountExpiryWithGrace(&original, nil, 30)
	extra[AccountDeliveryKeyExtraKey] = "legacy"
	extra[AccountDeliveryExpiryExtraKey] = original
	at := time.Unix(*deadline, 0)
	return &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, AutoPauseOnExpired: true, ExpiresAt: &at, Extra: extra}
}

func TestNativeExpiryDeliveryViewAndRoundTrip(t *testing.T) {
	a := nativeExpiryFixture()
	ctx := adminaccess.WithLegacyKey(context.Background())
	original := expiryTimestamp(a.Extra[AccountDeliveryExpiryExtraKey])
	require.Equal(t, original+1800, a.ExpiresAt.Unix())
	require.Equal(t, a.ExpiresAt.Unix(), a.SchedulingExpiresAt().Unix())
	require.True(t, a.IsSchedulable())
	require.Equal(t, original, AccountForAdminRead(ctx, a).ExpiresAt.Unix())
	require.Equal(t, original+1800, AccountForAdminRead(context.Background(), a).ExpiresAt.Unix())
	other := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{KeyID: "other"})
	require.Equal(t, original+1800, AccountForAdminRead(other, a).ExpiresAt.Unix())
	for i := 0; i < 3; i++ {
		ApplyAccountExpiryWrite(ctx, a, original)
		require.Equal(t, original+1800, a.ExpiresAt.Unix())
	}
	manual := original + 7200
	ApplyAccountExpiryWrite(context.Background(), a, manual)
	ApplyAccountExpiryWrite(ctx, a, original)
	require.Equal(t, manual, a.ExpiresAt.Unix())
	require.Equal(t, manual, a.SchedulingExpiresAt().Unix())
	require.Equal(t, original, AccountForAdminRead(ctx, a).ExpiresAt.Unix())
	ApplyAccountExpiryExtra(a, map[string]any{AccountSourceExpiryExtraKey: original - 60})
	require.Equal(t, manual, a.ExpiresAt.Unix(), "lease sync must respect a manual deadline")
	ApplyAccountExpiryExtra(a, map[string]any{AccountImportExpiryGraceExtraKey: 900})
	require.Equal(t, original-60+900, a.ExpiresAt.Unix())
	require.False(t, a.IsSchedulable(), "native expiry must not receive grace twice")
	require.False(t, a.Extra[AccountExpiryManualExtraKey].(bool))
}

func TestNativeExpiryScopedImportIdentityAndRenewal(t *testing.T) {
	a := nativeExpiryFixture()
	a.Extra[AccountDeliveryKeyExtraKey] = "scoped:importer"
	ctx := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{KeyID: "importer"})
	original := expiryTimestamp(a.Extra[AccountDeliveryExpiryExtraKey])
	require.Equal(t, original, AccountForAdminRead(ctx, a).ExpiresAt.Unix())
	ApplyAccountExpiryWrite(ctx, a, original+3600)
	require.Equal(t, original+3600+1800, a.ExpiresAt.Unix())
	require.Equal(t, original+3600, AccountForAdminRead(ctx, a).ExpiresAt.Unix())
	source, origin := a.SourceExpiresAt()
	require.Equal(t, original+3600, source.Unix())
	require.Equal(t, "import", origin)
	ApplyAccountExpiryWrite(context.Background(), a, 0)
	require.Nil(t, a.ExpiresAt)
	ApplyAccountExpiryWrite(ctx, a, original+3600)
	require.Nil(t, a.ExpiresAt)
}

func TestNativeExpiryLockedMergeProtectsConcurrentEdits(t *testing.T) {
	current := nativeExpiryFixture()
	stale := *current
	stale.Extra = map[string]any{"public": "keep", AccountDeliveryKeyExtraKey: "scoped:forged"}
	ApplyAccountExpiryExtra(current, map[string]any{AccountImportExpiryGraceExtraKey: 7200})
	MergeLockedAccountExpiry(context.Background(), &stale, current)
	require.Equal(t, current.ExpiresAt.Unix(), stale.ExpiresAt.Unix())
	require.Equal(t, 7200, stale.ExpiryGraceSeconds())
	require.Equal(t, "legacy", stale.Extra[AccountDeliveryKeyExtraKey])
	require.Equal(t, "keep", stale.Extra["public"])
	manual := current.ExpiresAt.Add(-time.Hour).Unix()
	MergeLockedAccountExpiry(WithAccountExpiryWrite(context.Background(), &manual), &stale, current)
	require.Equal(t, manual, stale.ExpiresAt.Unix())
	require.Equal(t, true, stale.Extra[AccountExpiryManualExtraKey])
	for key := range NativeAccountExtra(stale.Extra) {
		require.False(t, IsAccountExpiryExtra(key), key)
	}
}

func TestNativeExpiryMaterializationPreservesLegacyDeadline(t *testing.T) {
	base := time.Now().Add(-20 * time.Minute)
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExpiresAt: &base,
		Extra: map[string]any{AccountImportExpiryGraceExtraKey: 1800}}
	want := a.SchedulingExpiresAt().Unix()
	MaterializeAccountExpiry(a)
	require.Equal(t, want, a.ExpiresAt.Unix())
	require.Equal(t, want, a.SchedulingExpiresAt().Unix())
	MaterializeAccountExpiry(a)
	require.Equal(t, want, a.ExpiresAt.Unix())
}
