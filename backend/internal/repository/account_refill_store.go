package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountRefillStore struct{ db *sql.DB }

func NewAccountRefillStore(db *sql.DB) service.AccountRefillStore { return &accountRefillStore{db: db} }
func (s *accountRefillStore) Read(ctx context.Context) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", service.AccountRefillSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return raw, err
}
func (s *accountRefillStore) WithLock(ctx context.Context, fn func(string, func(string) error) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	const lockID int64 = 8362461180561
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return service.ErrAccountRefillBusy
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var released bool
		if err := conn.QueryRowContext(releaseCtx, "SELECT pg_advisory_unlock($1)", lockID).Scan(&released); err != nil || !released {
			// Never return a connection with a possibly held session lock to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var raw string
	err = conn.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", service.AccountRefillSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	save := func(value string) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW()) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, service.AccountRefillSettingKey, value)
		return err
	}
	return fn(raw, save)
}
