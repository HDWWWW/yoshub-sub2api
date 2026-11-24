package service

import (
	"context"
	"errors"
	"time"
)

// Both download deliveries and supplier-pushed accounts use the same saved policy.
func (s *AccountRefillService) refillDeliveryInput(ctx context.Context, j *AccountRefillJob, delivery *RefillDelivery) (*CreateAccountInput, error) {
	return s.buildRefillDeliveryInput(ctx, j, delivery, false)
}

// allowExpired is used only to record a confirmed, already ended pushed order.
// Such input must never pass through the normal account activation path.
func (s *AccountRefillService) buildRefillDeliveryInput(ctx context.Context, j *AccountRefillJob, delivery *RefillDelivery, allowExpired bool) (*CreateAccountInput, error) {
	if err := validateRefillDelivery(delivery); err != nil {
		return nil, err
	}
	a := delivery.Accounts[0]
	credentials := map[string]any{}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "client_id", "expires_at", "email", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "plan_type"} {
		if value, ok := a.Credentials[key].(string); ok && value != "" {
			credentials[key] = value
		}
	}
	if value, ok := a.Credentials["expires_at"].(float64); ok {
		credentials["expires_at"] = value
	}
	if credentials["access_token"] == nil {
		return nil, errors.New("交付缺少接入凭据")
	}
	deadline := a.ExpiresAt
	if j.ExpiresAt != nil && (deadline == nil || *j.ExpiresAt < *deadline) {
		deadline = j.ExpiresAt
	}
	if deadline == nil || *deadline <= 0 || *deadline > 253402214399 || (!allowExpired && *deadline <= time.Now().Unix()) {
		return nil, errors.New("交付缺少有效租期")
	}
	if j.DeliveryConfig.ExpiryMode == "fixed" && j.DeliveryConfig.FixedExpiresAt != nil && *j.DeliveryConfig.FixedExpiresAt < *deadline {
		deadline = j.DeliveryConfig.FixedExpiresAt
	}
	if !allowExpired && *deadline <= time.Now().Unix() {
		return nil, errors.New("指定截止时间已过期")
	}
	extra := map[string]any{"same_account_retry_count": j.DeliveryConfig.sameAccountRetryCount(), "refill_job_id": j.ID, "refill_supplier_id": j.SupplierID, "import_source": "llmrefund_api",
		"refill_plan":            j.Plan,
		"codex_fingerprint_mode": j.DeliveryConfig.FingerprintMode, "openai_oauth_responses_websockets_v2_enabled": j.DeliveryConfig.WSMode != "off", "openai_oauth_responses_websockets_v2_mode": j.DeliveryConfig.WSMode}
	if j.Provider == refillProviderMuze {
		extra["import_source"] = "muze_api"
	}
	if j.DeliveryConfig.OpenAIResponsesFlattenNamespaces != nil && *j.DeliveryConfig.OpenAIResponsesFlattenNamespaces {
		extra["openai_responses_flatten_namespaces"] = true
	}
	graceMinutes := AccountImportExpiryOffsetMinutes
	if j.ExpiryGraceMinutes != nil {
		graceMinutes = *j.ExpiryGraceMinutes
	}
	deadline, extra = ImportedAccountExpiryWithGrace(deadline, extra, graceMinutes)
	// The supplier lease is independent of a locally shortened deadline.
	if a.ExpiresAt != nil {
		extra[AccountSourceExpiryExtraKey] = *a.ExpiresAt
	}
	if j.ExpiresAt != nil {
		extra[AccountSourceExpiryExtraKey] = *j.ExpiresAt
	}
	if j.Provider == "" {
		// 1208 may send the supplier lease only with the delivered account.
		if source, ok := extra[AccountSourceExpiryExtraKey].(int64); ok && source > 0 && source <= 253402214399 {
			j.SupplierExpiresAt = &source
		}
	}
	extra[AccountSourceExpiryOriginExtraKey] = "supplier_api"
	extra[AccountSourceExpiryCheckedExtraKey] = time.Now().UTC().Format(time.RFC3339)
	if len(j.DeliveryConfig.ModelMapping) > 0 {
		credentials["model_mapping"] = j.DeliveryConfig.ModelMapping
	}
	email, _ := credentials["email"].(string)
	name := a.Name
	if name == "" {
		name = email
	}
	if name == "" {
		name = "补号 " + j.SupplierID
	}
	expires := time.Unix(*deadline, 0)
	pushTime := time.Now()
	if j.DeliveryConfig.NameTemplate != "" {
		name = refillTemplate(j.DeliveryConfig.NameTemplate, name, email, j.SupplierID, j.Plan, pushTime, expires)
	}
	var notes *string
	if j.DeliveryConfig.NotesTemplate != "" {
		n := refillTemplate(j.DeliveryConfig.NotesTemplate, a.Name, email, j.SupplierID, j.Plan, pushTime, expires)
		notes = &n
	}
	autoPause := j.DeliveryConfig.AutoPauseOnExpired
	var proxyID *int64
	if !allowExpired && j.DeliveryConfig.ProxyMode != "random" && j.DeliveryConfig.ProxyMode != "ovh_ipv6" {
		var err error
		proxyID, err = s.selectProxy(ctx, j.DeliveryConfig)
		if err != nil {
			return nil, err
		}
	}
	var load *int
	if j.DeliveryConfig.LoadFactor > 0 {
		v := j.DeliveryConfig.LoadFactor
		load = &v
	}
	// Start disabled until all delivery settings and bindings have committed.
	input := &CreateAccountInput{Name: name, Notes: notes, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: credentials, Extra: extra,
		PrivacyEnabled: j.DeliveryConfig.PrivacyEnabled,
		GroupIDs:       j.DeliveryGroupIDs, Concurrency: j.Concurrency, Priority: j.DeliveryConfig.Priority, RateMultiplier: &j.DeliveryConfig.RateMultiplier, LoadFactor: load,
		ExpiresAt: deadline, AutoPauseOnExpired: &autoPause, ProxyID: proxyID, InitiallyInactive: true, SkipDefaultGroupBind: true}

	return input, nil
}
