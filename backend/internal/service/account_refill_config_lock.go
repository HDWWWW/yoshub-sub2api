package service

import (
	"context"
	"errors"
	"time"
)

// Configuration saves may arrive while the worker is checking a supplier.
// Wait for that existing operation instead of rejecting a normal save at once.
// Worker/order paths retain their nonblocking, cross-instance advisory lock.
func withAccountRefillConfigLock(ctx context.Context, store AccountRefillStore, maxWait time.Duration, fn func(string, func(string) error) error) error {
	deadline := time.Now().Add(maxWait)
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrAccountRefillBusy
		}
		entered := false
		err := store.WithLock(ctx, func(raw string, save func(string) error) error {
			entered = true
			if err := ctx.Err(); err != nil {
				return err
			}
			if !time.Now().Before(deadline) {
				return ErrAccountRefillBusy
			}
			return fn(raw, save)
		})
		// Never replay a callback that has started: it can perform provider
		// validation or a durable save even if its result is uncertain.
		if entered || !errors.Is(err, ErrAccountRefillBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrAccountRefillBusy
		case <-ticker.C:
		}
	}
}
