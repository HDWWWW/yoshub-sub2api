package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountRefillStore struct {
	db      *sql.DB
	groupID int64
}

func (s *accountRefillStore) key() string {
	if s.groupID == 0 {
		return service.AccountRefillSettingKey
	}
	return service.AccountRefillSettingKey + ":group:" + strconv.FormatInt(s.groupID, 10)
}

func (s *accountRefillStore) ForGroup(id int64) service.AccountRefillStore {
	return &accountRefillStore{db: s.db, groupID: id}
}

func (s *accountRefillStore) ListRules(ctx context.Context) (map[int64]string, error) {
	prefix := service.AccountRefillSettingKey + ":group:"
	rows, err := s.db.QueryContext(ctx, "SELECT key,value FROM settings WHERE key=$1 OR starts_with(key,$2) ORDER BY key", service.AccountRefillSettingKey, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		id := int64(0)
		if key != service.AccountRefillSettingKey {
			id, err = strconv.ParseInt(strings.TrimPrefix(key, prefix), 10, 64)
			if err != nil || id <= 0 {
				continue
			}
		}
		out[id] = value
	}
	return out, rows.Err()
}

func NewAccountRefillStore(db *sql.DB) service.AccountRefillStore { return &accountRefillStore{db: db} }
func (s *accountRefillStore) Read(ctx context.Context) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", s.key()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return raw, err
}
func (s *accountRefillStore) WithLock(ctx context.Context, fn func(string, func(string) error) error) error {
	// During a state-format upgrade, the new slot must not write fields that an
	// old worker would discard. A slot-specific deployment marker gates only
	// refill mutations until the old slot drains; no database lock is held.
	if pauseFile := strings.TrimSpace(os.Getenv("SUB2API_REFILL_PAUSE_FILE")); pauseFile != "" {
		if _, err := os.Stat(pauseFile); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: path comes only from the operator's process environment, never request data.
			return service.ErrAccountRefillBusy
		}
	}
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
	err = conn.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", s.key()).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	save := func(value string) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW()) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, s.key(), value)
		return err
	}
	return fn(raw, save)
}
