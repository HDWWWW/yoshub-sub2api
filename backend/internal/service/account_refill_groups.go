package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"
)

type AccountRefillRuleSummary struct {
	GroupID         int64  `json:"group_id"`
	Enabled         bool   `json:"enabled"`
	MonitorEnabled  bool   `json:"monitor_enabled"`
	RotationEnabled bool   `json:"rotation_enabled"`
	NextPlan        string `json:"next_plan"`
}

// New rules inherit only the connection. Automation starts disabled; budgets,
// jobs, delivery settings and rotation progress are never copied.
type seededRefillGroupStore struct {
	AccountRefillStore
	root    AccountRefillStore
	groupID int64
}

func (s *seededRefillGroupStore) seed(ctx context.Context, raw string) (string, error) {
	if raw != "" {
		return raw, nil
	}
	base, err := s.root.Read(ctx)
	if err != nil {
		return "", err
	}
	old, err := decodeAccountRefillState(base)
	if err != nil {
		return "", err
	}
	if old.EncryptedToken == "" {
		if stores, ok := s.root.(AccountRefillGroupStore); ok {
			rows, err := stores.ListRules(ctx)
			if err != nil {
				return "", err
			}
			ids := make([]int64, 0, len(rows))
			for id := range rows {
				if id > 0 && id != s.groupID {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			for _, id := range ids {
				candidate, err := decodeAccountRefillState(rows[id])
				if err != nil {
					return "", err
				}
				if candidate.EncryptedToken != "" {
					old = candidate
					break
				}
			}
		}
	}
	state, _ := decodeAccountRefillState("")
	state.Config.GroupID = s.groupID
	state.Config.DeliveryGroupIDs = []int64{s.groupID}
	state.EncryptedToken, state.EncryptedWebhook = old.EncryptedToken, old.EncryptedWebhook
	state.Key, state.Wallet, state.Catalog = old.Key, old.Wallet, old.Catalog
	state.IdentityAt, state.CatalogAt = old.IdentityAt, old.CatalogAt
	b, err := json.Marshal(state)
	return string(b), err
}
func (s *seededRefillGroupStore) Read(ctx context.Context) (string, error) {
	raw, err := s.AccountRefillStore.Read(ctx)
	if err != nil {
		return "", err
	}
	return s.seed(ctx, raw)
}
func (s *seededRefillGroupStore) WithLock(ctx context.Context, fn func(string, func(string) error) error) error {
	return s.AccountRefillStore.WithLock(ctx, func(raw string, save func(string) error) error {
		seed, err := s.seed(ctx, raw)
		if err != nil {
			return err
		}
		return fn(seed, save)
	})
}
func (s *AccountRefillService) rootService() *AccountRefillService {
	if s.ruleRoot != nil {
		return s.ruleRoot
	}
	return s
}
func (s *AccountRefillService) groupService(id int64, store AccountRefillGroupStore) *AccountRefillService {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	if s.ruleServices == nil {
		s.ruleServices = map[int64]*AccountRefillService{}
	}
	if child := s.ruleServices[id]; child != nil {
		return child
	}
	child := &AccountRefillService{
		store:     &seededRefillGroupStore{AccountRefillStore: store.ForGroup(id), root: s.store, groupID: id},
		encryptor: s.encryptor, accounts: s.accounts, admin: s.admin, groups: s.groups,
		loads: s.loads, proxies: s.proxies, provider: s.provider, notifier: s.notifier,
		ruleRoot: s, ruleGroupID: id,
	}
	s.ruleServices[id] = child
	return child
}
func (s *AccountRefillService) ForGroup(ctx context.Context, id int64) (*AccountRefillService, error) {
	s = s.rootService()
	if id == 0 {
		return s, nil
	}
	if id < 0 {
		return nil, errors.New("分组 ID 无效")
	}
	raw, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	state, err := decodeAccountRefillState(raw)
	if err != nil {
		return nil, err
	}
	if id == state.Config.GroupID {
		return s, nil
	}
	store, ok := s.store.(AccountRefillGroupStore)
	if !ok {
		return nil, errors.New("分组规则存储未就绪")
	}
	g, err := s.groups.GetByID(ctx, id)
	if err != nil || g == nil || g.Platform != PlatformOpenAI {
		return nil, errors.New("请选择有效的 OpenAI 分组")
	}
	return s.groupService(id, store), nil
}
func (s *AccountRefillService) ListRules(ctx context.Context) ([]AccountRefillRuleSummary, error) {
	s = s.rootService()
	out := []AccountRefillRuleSummary{}
	store, ok := s.store.(AccountRefillGroupStore)
	if !ok {
		return out, nil
	}
	rows, err := store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, raw := range rows {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return nil, err
		}
		c := state.Config
		if c.GroupID > 0 {
			out = append(out, AccountRefillRuleSummary{GroupID: c.GroupID, Enabled: c.Enabled, MonitorEnabled: c.MonitorEnabled, RotationEnabled: c.RotationEnabled, NextPlan: nextRefillPurchasePlan(&state)})
		}
	}
	slices.SortFunc(out, func(a, b AccountRefillRuleSummary) int {
		if a.GroupID < b.GroupID {
			return -1
		}
		if a.GroupID > b.GroupID {
			return 1
		}
		return 0
	})
	return out, nil
}
func (s *AccountRefillService) allRuleServices(ctx context.Context) ([]*AccountRefillService, error) {
	out := []*AccountRefillService{s}
	store, ok := s.store.(AccountRefillGroupStore)
	if !ok {
		return out, nil
	}
	rows, err := store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		out = append(out, s.groupService(id, store))
	}
	return out, nil
}
func (s *AccountRefillService) sampleAllCapacity(ctx context.Context) {
	services, err := s.allRuleServices(ctx)
	if err != nil {
		return
	} // Samples become stale and cannot authorize orders.
	var wg sync.WaitGroup
	for _, rule := range services {
		wg.Add(1)
		go func() { defer wg.Done(); rule.sampleCapacity(ctx) }()
	}
	wg.Wait()
}
func (s *AccountRefillService) runAllRules(ctx context.Context) error {
	services, err := s.allRuleServices(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, rule := range services {
		if ctx.Err() != nil {
			break
		}
		ruleCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := rule.run(ruleCtx, false)
		cancel()
		if err != nil && !errors.Is(err, ErrAccountRefillBusy) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Called inside the shared advisory lock before either manual or automatic
// reservations, preventing separate rules from spending the same wallet funds.
func (s *AccountRefillService) unsubmittedPoints(ctx context.Context, state *accountRefillState) (float64, error) {
	points := 0.0
	add := func(jobs []AccountRefillJob) {
		for _, j := range jobs {
			if j.pending() && j.SupplierID == "" {
				points += j.Price
			}
		}
	}
	add(state.Jobs)
	store, ok := s.rootService().store.(AccountRefillGroupStore)
	if !ok {
		return points, nil
	}
	rows, err := store.ListRules(ctx)
	if err != nil {
		return 0, errors.New("无法核对其他分组的预算预占，暂停新增补号")
	}
	for key, raw := range rows {
		if key == s.ruleGroupID {
			continue
		}
		other, err := decodeAccountRefillState(raw)
		if err != nil {
			return 0, err
		}
		if state.Wallet != nil && other.Wallet != nil && state.Wallet.CustomerID != "" && other.Wallet.CustomerID != "" && state.Wallet.CustomerID != other.Wallet.CustomerID {
			continue
		}
		add(other.Jobs)
	}
	return points, nil
}

// Delivery groups can overlap. An order from another rule that will arrive in
// this pool also covers its shortage, even though its budget belongs elsewhere.
func (s *AccountRefillService) evaluationJobs(ctx context.Context, state *accountRefillState) ([]AccountRefillJob, error) {
	jobs, _, err := s.evaluationInputs(ctx, state)
	return jobs, err
}

func (s *AccountRefillService) evaluationInputs(ctx context.Context, state *accountRefillState) ([]AccountRefillJob, []accountRefillExpiryDemand, error) {
	jobs := slices.Clone(state.Jobs)
	var claims []accountRefillExpiryDemand
	store, ok := s.rootService().store.(AccountRefillGroupStore)
	if !ok {
		return jobs, claims, nil
	}
	rows, err := store.ListRules(ctx)
	if err != nil {
		return nil, nil, errors.New("无法核对分组在途任务，暂停新增补号")
	}
	for key, raw := range rows {
		if key == s.ruleGroupID {
			continue
		}
		other, err := decodeAccountRefillState(raw)
		if err != nil {
			return nil, nil, err
		}
		for _, job := range other.Jobs {
			if (job.pending() || job.Status == "imported" || job.ReplacementPending) && slices.Contains(job.DeliveryGroupIDs, state.Config.GroupID) {
				jobs = append(jobs, job)
			}
		}
		for _, demand := range other.ExpiryDemands {
			if demand.JobID != "" && slices.Contains(demand.DeliveryGroupIDs, state.Config.GroupID) {
				claims = append(claims, demand)
			}
		}
	}
	return jobs, claims, nil
}
