package service

import "github.com/Wei-Shaw/sub2api/internal/config"

func ProvideAccountRiskService(store AccountRiskStore, accounts AccountRepository, groups GroupRepository, loads *ConcurrencyService, probe *AccountTestService, cfg *config.Config, refill *AccountRefillService, gateway *OpenAIGatewayService, counter429 AccountRisk429Counter) *AccountRiskService {
	s := NewAccountRiskService(store, accounts, groups, loads, probe, cfg)
	s.notifier = refill
	s.counter429 = counter429
	gateway.accountRisk = s
	s.Start()
	return s
}
