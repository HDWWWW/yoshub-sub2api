package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// AccountExpiryPriorityRepository atomically updates priorities and publishes scheduler changes.
type AccountExpiryPriorityRepository interface {
	SyncAccountExpiryPriorities(ctx context.Context, now time.Time) (int64, error)
}

// AccountExpiryService maintains expiry priorities and pauses expired accounts.
type AccountExpiryService struct {
	accountRepo  AccountRepository
	priorityRepo AccountExpiryPriorityRepository
	interval     time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

func NewAccountExpiryService(accountRepo AccountRepository, interval time.Duration) *AccountExpiryService {
	return &AccountExpiryService{
		accountRepo: accountRepo,
		interval:    interval,
		stopCh:      make(chan struct{}),
	}
}

func (s *AccountExpiryService) Start() {
	if s == nil || s.accountRepo == nil || s.interval <= 0 {
		return
	}
	if s.priorityRepo != nil {
		log.Printf("[AccountExpiry] Automatic expiry priority enabled (interval=%s)", s.interval)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountExpiryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *AccountExpiryService) runOnce() {
	// Use separate deadlines so either maintenance task can still run when the other fails.
	if s.priorityRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		updated, err := s.priorityRepo.SyncAccountExpiryPriorities(ctx, time.Now())
		cancel()
		if err != nil {
			log.Printf("[AccountExpiry] Sync expiry priorities failed: %v", err)
		} else if updated > 0 {
			log.Printf("[AccountExpiry] Updated expiry priorities for %d accounts", updated)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	updated, err := s.accountRepo.AutoPauseExpiredAccounts(ctx, time.Now())
	if err != nil {
		log.Printf("[AccountExpiry] Auto pause expired accounts failed: %v", err)
		return
	}
	if updated > 0 {
		log.Printf("[AccountExpiry] Auto paused %d expired accounts", updated)
	}
}
