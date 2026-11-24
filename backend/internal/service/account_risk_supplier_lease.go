package service

import (
	"context"
	"errors"
	"time"
)

var errRiskSupplierReclaimed = errors.New("已到号商预计回收时间，跳过自动风控探测")

// Either the scheduled account or the actual credential owner may have a lease.
func riskProbeDeadline(source, owner *Account) *time.Time {
	at, _ := source.supplierReclaimAt()
	if owner != nil && owner != source {
		other, _ := owner.supplierReclaimAt()
		if other != nil && (at == nil || other.Before(*at)) {
			return other
		}
	}
	return at
}

func supplierReclaimed(deadline *time.Time, at time.Time) bool {
	return deadline != nil && !at.Before(*deadline)
}

// Reuse accounts already in the group and batch-load only out-of-group parents.
func (s *AccountRiskService) probeCredentialOwners(ctx context.Context, group []Account) (map[int64]*Account, error) {
	owners := map[int64]*Account{}
	for i := range group {
		if a := &group[i]; a.Platform == PlatformOpenAI && a.IsShadow() {
			owners[*a.ParentAccountID] = nil
		}
	}
	if len(owners) == 0 {
		return nil, nil
	}
	for i := range group {
		if _, wanted := owners[group[i].ID]; wanted {
			owners[group[i].ID] = &group[i]
		}
	}
	var missing []int64
	for id, owner := range owners {
		if owner == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		rows, err := s.accounts.GetByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		for _, owner := range rows {
			if owner != nil {
				owners[owner.ID] = owner
			}
		}
	}
	return owners, nil
}

func (s *AccountRiskService) accountProbeDeadline(ctx context.Context, source *Account) (*time.Time, error) {
	var owner *Account
	if source.IsShadow() {
		var err error
		owner, err = s.accounts.GetByID(ctx, *source.ParentAccountID)
		if err != nil {
			return nil, err
		}
		if owner == nil {
			return nil, ErrAccountNotFound
		}
	}
	return riskProbeDeadline(source, owner), nil
}

func (s *AccountRiskService) runProbeWithinLease(ctx context.Context, account *Account, config AccountRiskConfig) (AccountRiskProbeResult, error) {
	deadline, err := s.accountProbeDeadline(ctx, account)
	if err != nil {
		return AccountRiskProbeResult{}, err
	}
	if deadline != nil {
		if supplierReclaimed(deadline, time.Now()) {
			return AccountRiskProbeResult{}, errRiskSupplierReclaimed
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(ctx, *deadline, errRiskSupplierReclaimed)
		defer cancel()
	}
	result, err := s.probe.RunRiskProbe(ctx, account, config)
	if errors.Is(context.Cause(ctx), errRiskSupplierReclaimed) {
		// A 401 received just before reclaim can be queued after the timer fires.
		if e := result.auth401; e == nil || e.receivedAt.IsZero() || supplierReclaimed(deadline, e.receivedAt) {
			return AccountRiskProbeResult{}, errRiskSupplierReclaimed
		}
	}
	return result, err
}

// A response can outlive a scheduler snapshot. In particular, a pending Muze
// lease may have been synchronized during the request. Use the time of the
// rejection and the parent whose credentials were used for this attempt.
func (s *AccountRiskService) risk401Reclaimed(ctx context.Context, source *Account, ownerID int64, at time.Time) (bool, error) {
	if !source.IsOpenAIOAuth() && !source.IsShadow() {
		return false, nil
	}
	if ownerID == 0 && source.IsShadow() {
		ownerID = *source.ParentAccountID
	}
	current, err := s.accounts.GetByID(ctx, source.ID)
	if err != nil && !errors.Is(err, ErrAccountNotFound) {
		return false, err
	}
	if current != nil {
		source = current
	}
	return s.known401Reclaimed(ctx, source, ownerID, at)
}

func (s *AccountRiskService) known401Reclaimed(ctx context.Context, source *Account, ownerID int64, at time.Time) (bool, error) {
	var owner *Account
	var err error
	if ownerID != 0 && ownerID != source.ID {
		owner, err = s.accounts.GetByID(ctx, ownerID)
		if err != nil && !errors.Is(err, ErrAccountNotFound) {
			return false, err
		}
	}
	return supplierReclaimed(riskProbeDeadline(source, owner), at), nil
}

// Check old pending events and leases synchronized since enqueue. A delayed
// delivery uses the rejection time, keeping genuine pre-reclaim alerts.
func (s *AccountRiskService) queued401Reclaimed(ctx context.Context, e *AccountRiskEvent) (bool, error) {
	if e.Kind != accountRiskBusiness401 && e.Kind != accountRiskProbe401 {
		return false, nil
	}
	a, err := s.accounts.GetByID(ctx, e.AccountID)
	if errors.Is(err, ErrAccountNotFound) {
		return false, nil
	}
	if err != nil || a == nil {
		return false, err
	}
	var parentID int64
	if a.IsShadow() {
		parentID = *a.ParentAccountID
	}
	return s.known401Reclaimed(ctx, a, parentID, e.At)
}
