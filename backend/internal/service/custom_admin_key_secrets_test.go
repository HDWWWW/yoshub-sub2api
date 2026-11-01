//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestScopedAdminKeyCopyLifecycle(t *testing.T) {
	ctx := context.Background()
	keys := &scopedKeyMemoryRepo{}
	storage := newMockSettingRepo()
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: true}}
	service := &SettingService{scopedAdminKeyRepo: keys, settingRepo: storage, cfg: cfg, scopedAdminKeyEncryptor: &plainEncryptor{}}
	key, item, err := service.CreateScopedAdminKey(ctx, ScopedAdminKeyInput{Name: "copy", Enabled: true}, 1)
	require.NoError(t, err)
	require.Equal(t, "ENC:"+key, storage.data[scopedAdminKeySecretPrefix+item.Hash])
	items, err := service.ListScopedAdminKeys(ctx)
	require.NoError(t, err)
	listed, err := json.Marshal(items)
	require.NoError(t, err)
	require.NotContains(t, string(listed), key)
	require.NotContains(t, string(listed), item.Hash)
	settings, err := service.GetAllSettings(ctx)
	require.NoError(t, err)
	publicSettings, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NotContains(t, string(publicSettings), "ENC:"+key)
	require.NotContains(t, string(publicSettings), key)
	// A fresh service sees the same secret: copying survives process restarts.
	reader := &SettingService{scopedAdminKeyRepo: keys, settingRepo: storage, cfg: cfg, scopedAdminKeyEncryptor: &plainEncryptor{}}
	for range 2 {
		copied, err := reader.RevealScopedAdminKey(ctx, item.ID)
		require.NoError(t, err)
		require.Equal(t, key, copied)
	}
	keys.item.Enabled = false
	copied, err := reader.RevealScopedAdminKey(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, key, copied)
	require.False(t, keys.item.Enabled, "copy must not enable a disabled key")
	next, rotated, err := service.RotateScopedAdminKey(ctx, item.ID)
	require.NoError(t, err)
	require.NotEqual(t, key, next)
	require.NotContains(t, storage.data, scopedAdminKeySecretPrefix+item.Hash)
	copied, err = reader.RevealScopedAdminKey(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, next, copied)
	_, err = reader.AuthenticateScopedAdminKey(ctx, key, "127.0.0.1")
	require.ErrorIs(t, err, ErrScopedAdminKeyInvalid)
	require.NoError(t, service.RevokeScopedAdminKey(ctx, item.ID))
	require.NotContains(t, storage.data, scopedAdminKeySecretPrefix+rotated.Hash)
	copied, err = reader.RevealScopedAdminKey(ctx, item.ID)
	require.Empty(t, copied)
	require.ErrorIs(t, err, ErrScopedAdminKeyNotFound)
}

type failingAdminSecretStore struct{ *mockSettingRepo }

func (s *failingAdminSecretStore) Set(context.Context, string, string) error {
	return errors.New("storage unavailable")
}

func TestScopedAdminKeyCopyFailureSafety(t *testing.T) {
	ctx := context.Background()
	keys := &scopedKeyMemoryRepo{}
	storage := newMockSettingRepo()
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: true}}
	s := &SettingService{scopedAdminKeyRepo: keys, settingRepo: storage, cfg: cfg, scopedAdminKeyEncryptor: &plainEncryptor{}}
	key, item, err := s.CreateScopedAdminKey(ctx, ScopedAdminKeyInput{Name: "copy", Enabled: true}, 1)
	require.NoError(t, err)
	settingsKey := scopedAdminKeySecretPrefix + item.Hash
	storage.data[settingsKey] = "ENC:another-key"
	copied, err := s.RevealScopedAdminKey(ctx, item.ID)
	require.Error(t, err)
	require.Empty(t, copied, "mismatched ciphertext must never be returned")
	delete(storage.data, settingsKey)
	_, err = s.RevealScopedAdminKey(ctx, item.ID)
	require.ErrorIs(t, err, ErrScopedAdminKeySecretUnavailable)
	s.settingRepo = &failingAdminSecretStore{storage}
	_, _, err = s.RotateScopedAdminKey(ctx, item.ID)
	require.Error(t, err)
	require.Equal(t, item.Hash, keys.item.Hash, "failed persistence must not rotate the key")
	_, err = s.AuthenticateScopedAdminKey(ctx, key, "127.0.0.1")
	require.NoError(t, err)
	s.settingRepo = storage
	cfg.Totp.EncryptionKeyConfigured = false
	_, _, err = s.CreateScopedAdminKey(ctx, ScopedAdminKeyInput{Name: "unsafe", Enabled: true}, 1)
	require.ErrorIs(t, err, ErrScopedAdminKeyEncryptionNotConfigured)
	require.Equal(t, item.ID, keys.item.ID)
}
