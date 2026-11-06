package service

import "github.com/Wei-Shaw/sub2api/internal/config"

func ProvideAccountRiskService(store AccountRiskStore, accounts AccountRepository, groups GroupRepository, loads *ConcurrencyService, probe *AccountTestService, cfg *config.Config) *AccountRiskService {
	s := NewAccountRiskService(store, accounts, groups, loads, probe, cfg)
	s.Start()
	return s
}
