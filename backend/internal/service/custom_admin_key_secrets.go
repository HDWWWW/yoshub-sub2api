package service

import (
	"context"
	"errors"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// A hash-qualified settings entry keeps old/new versions compatible during
// blue-green deployment, without changing the authentication table or schema.
// Rotations cannot overwrite another rotation's secret; reads verify the hash.
const scopedAdminKeySecretPrefix = "scoped_admin_key_secret:"

var ErrScopedAdminKeySecretUnavailable = infraerrors.Conflict("ADMIN_KEY_SECRET_UNAVAILABLE", "此密钥未保存完整值，请轮换后复制")
var ErrScopedAdminKeyEncryptionNotConfigured = infraerrors.BadRequest("ADMIN_KEY_ENCRYPTION_NOT_CONFIGURED", "请先配置固定的密钥加密配置 TOTP_ENCRYPTION_KEY")

func (s *SettingService) storeScopedAdminKeySecret(ctx context.Context, key, hash string) error {
	// Legacy service consumers can continue to issue hash-only keys. The
	// application provider always injects the AES-GCM encryptor.
	if s.scopedAdminKeyEncryptor == nil {
		return nil
	}
	if s.cfg == nil || !s.cfg.Totp.EncryptionKeyConfigured || s.settingRepo == nil {
		return ErrScopedAdminKeyEncryptionNotConfigured
	}
	ciphertext, err := s.scopedAdminKeyEncryptor.Encrypt(key)
	if err != nil {
		return infraerrors.InternalServer("ADMIN_KEY_ENCRYPTION_FAILED", "无法安全保存密钥")
	}
	return s.settingRepo.Set(ctx, scopedAdminKeySecretPrefix+hash, ciphertext)
}

func (s *SettingService) discardScopedAdminKeySecret(ctx context.Context, hash string) {
	if s.scopedAdminKeyEncryptor == nil || s.settingRepo == nil {
		return
	}
	// Auth already rejects the discarded hash. Cleanup failure must not turn a
	// completed rotation/revocation into an ambiguous error for the caller.
	if err := s.settingRepo.Delete(ctx, scopedAdminKeySecretPrefix+hash); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: admin key secret cleanup failed")
	}
}

func (s *SettingService) RevealScopedAdminKey(ctx context.Context, id string) (string, error) {
	r, err := s.scopedAdminKeys()
	if err != nil {
		return "", err
	}
	item, err := r.GetScopedAdminKey(ctx, id)
	if err != nil {
		return "", err
	}
	if item.RevokedAt != nil {
		return "", ErrScopedAdminKeyNotFound
	}
	if s.scopedAdminKeyEncryptor == nil || s.settingRepo == nil {
		return "", ErrScopedAdminKeySecretUnavailable
	}
	if s.cfg == nil || !s.cfg.Totp.EncryptionKeyConfigured {
		return "", ErrScopedAdminKeyEncryptionNotConfigured
	}
	ciphertext, err := s.settingRepo.GetValue(ctx, scopedAdminKeySecretPrefix+item.Hash)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && ciphertext == "") {
		return "", ErrScopedAdminKeySecretUnavailable
	}
	if err != nil {
		return "", err
	}
	key, err := s.scopedAdminKeyEncryptor.Decrypt(ciphertext)
	if err != nil || !verifyScopedAdminKey(key, item.Hash) {
		return "", infraerrors.InternalServer("ADMIN_KEY_DECRYPTION_FAILED", "密钥读取失败，请检查加密配置")
	}
	current, err := r.GetScopedAdminKey(ctx, id)
	if err != nil {
		return "", err
	}
	if current.RevokedAt != nil || current.Hash != item.Hash {
		return "", ErrScopedAdminKeyConflict
	}
	return key, nil
}
