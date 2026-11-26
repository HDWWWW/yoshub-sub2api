package service

// An absent filter preserves legacy notification behavior. An explicit empty
// object disables every type without changing detection or scheduling actions.
type AccountRiskNotificationTypes struct {
	Capacity          bool `json:"capacity"`
	Business429       bool `json:"business_429"`
	CustomError       bool `json:"custom_error"`
	ScheduledMismatch bool `json:"scheduled_mismatch"`
	Scheduled429      bool `json:"scheduled_429"`
	Auth401           bool `json:"auth_401"`
	ModelMismatch     bool `json:"model_mismatch"`
}

func riskNotificationChannel(c AccountRiskConfig) string {
	if c.NotificationChannel == "" {
		return "wecom"
	}
	return c.NotificationChannel
}

func riskNotificationTypeEnabled(c AccountRiskConfig, event AccountRiskEvent) bool {
	types := c.NotificationTypes
	if types == nil {
		return true
	}
	switch event.Kind {
	case accountRiskProbeCapacity:
		return types.Capacity
	case "rate_limit_429":
		return types.Business429
	case "custom_error":
		return types.CustomError
	case accountRiskProbeSingle, accountRiskProbeAll:
		return types.ScheduledMismatch
	case accountRiskProbeSingle + "_429", accountRiskProbeAll + "_429",
		accountRiskProbeSingle + "_mismatch_429", accountRiskProbeAll + "_mismatch_429":
		return types.Scheduled429
	case accountRiskProbe401, accountRiskBusiness401, accountRiskAccount401:
		return types.Auth401
	case "model_mismatch":
		return types.ModelMismatch
	case "":
		// Legacy untyped events cannot be classified reliably. Only retain them
		// when every type is allowed, so exclusions cannot leak an old alert.
		return types.Capacity && types.Business429 && types.CustomError && types.ScheduledMismatch && types.Scheduled429 && types.Auth401 && types.ModelMismatch
	default:
		return false
	}
}

func filterRiskNotification(c AccountRiskConfig, event *AccountRiskEvent) {
	if event.Notification == "pending" && !riskNotificationTypeEnabled(c, *event) {
		event.Notification = "disabled"
	}
}
