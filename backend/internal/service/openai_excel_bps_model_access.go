package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var excelBPSModelID = regexp.MustCompile(`^gpt-[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Convert only the official access response. Empty/denied access is authoritative;
// malformed envelopes and HTTP failures cannot manufacture a local allowlist.
func excelBPSAccessModelsBody(raw []byte) ([]byte, error) {
	var envelope struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models     *[]json.RawMessage `json:"models"`
			Restricted []string           `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Allowed == nil {
		return nil, fmt.Errorf("invalid Excel BPS access response")
	}
	models := make([]map[string]any, 0)
	if *envelope.Allowed {
		if envelope.Catalog == nil || envelope.Catalog.Models == nil {
			return nil, fmt.Errorf("missing Excel BPS model catalog")
		}
		restricted := map[string]bool{}
		for _, id := range envelope.Catalog.Restricted {
			restricted[id] = true
		}
		seen := map[string]bool{}
		for _, raw := range *envelope.Catalog.Models {
			var entry struct {
				ID        string `json:"id"`
				Label     string `json:"label"`
				Available *bool  `json:"available"`
				Enabled   *bool  `json:"enabled"`
				Picker    *bool  `json:"model_picker_enabled"`
				Policy    *struct {
					State string `json:"state"`
				} `json:"policy"`
				Efforts []struct {
					Value string `json:"value"`
				} `json:"efforts"`
			}
			if json.Unmarshal(raw, &entry) != nil || !excelBPSModelID.MatchString(entry.ID) || restricted[entry.ID] || seen[entry.ID] {
				continue
			}
			if entry.Available != nil && !*entry.Available || entry.Enabled != nil && !*entry.Enabled || entry.Picker != nil && !*entry.Picker || entry.Policy != nil && (entry.Policy.State == "disabled" || entry.Policy.State == "unconfigured") {
				continue
			}
			model := map[string]any{"id": entry.ID, "object": "model", "owned_by": "openai", "created": 0}
			if label := strings.TrimSpace(entry.Label); label != "" {
				model["display_name"] = label
			}
			efforts := make([]string, 0)
			effortSeen := map[string]bool{}
			for _, effort := range entry.Efforts {
				switch effort.Value {
				case "low", "medium", "high", "xhigh":
					if !effortSeen[effort.Value] {
						efforts = append(efforts, effort.Value)
						effortSeen[effort.Value] = true
					}
				}
			}
			if entry.Efforts != nil {
				model["reasoning_efforts"] = efforts
				model["supported_reasoning_levels"] = efforts
			}
			models = append(models, model)
			seen[entry.ID] = true
		}
	}
	return json.Marshal(map[string]any{"object": "list", "data": models})
}

// Permission catalogs are cached only while fresh, separately from Codex.
func (s *OpenAIGatewayService) FetchExcelBPSModelsList(ctx context.Context, account *Account) (*OpenAIModelsResponse, error) {
	if s == nil || account == nil || !account.IsExcelBPSEnabled() || !s.excelBPSGloballyEnabled(ctx) {
		return nil, errExcelOAuthRouteUnavailable
	}
	if account.IsExcelBPSMihomoEnabled() || (account.ProxyID != nil && account.Proxy == nil) {
		return nil, errExcelBPSProxyUnavailable
	}
	token, err := s.getExcelBPSAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return nil, fmt.Errorf("excel BPS discovery requires an account ID")
	}
	request, err := newExcelBPSRequest(ctx, nil, token, accountID)
	if err != nil {
		return nil, err
	}
	request.Method, request.Body, request.ContentLength = http.MethodGet, nil, 0
	request.Header.Set("Accept", "application/json")
	request.URL.Path, request.URL.RawQuery = "/basispoints/api/responses/access", "include_models=true"
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	key := buildOpenAIModelsCacheKey(openAIModelsRequest{url: request.URL.String(), headers: request.Header, proxyURL: proxy, accountID: account.ID, credentialAccountID: account.ID, standardModelsList: true})
	if cached, state := s.excelBPSModelsCache.get(key, time.Now()); state == openAIModelsCacheFresh {
		return excelBPSSelectedModels(cached, account)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := s.doExcelBPSHTTP(request.WithContext(ctx), proxy, account)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, infraerrors.New(502, "EXCEL_BPS_MODELS_UPSTREAM_FAILED", "excel BPS model discovery failed")
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("excel BPS model discovery returned no response")
	}
	defer func() { _ = response.Body.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stop()
	if response.StatusCode != http.StatusOK {
		return nil, infraerrors.New(502, "EXCEL_BPS_MODELS_UPSTREAM_REJECTED", fmt.Sprintf("Excel BPS model discovery returned HTTP %d", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, openAIModelsCacheBodyLimit+1))
	if err != nil || len(raw) > openAIModelsCacheBodyLimit {
		return nil, fmt.Errorf("excel BPS catalog could not be read")
	}
	body, err := excelBPSAccessModelsBody(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &OpenAIModelsResponse{Body: body, ETag: codexModelsManifestBodyETag(body)}
	s.excelBPSModelsCache.set(key, result, time.Now())
	return excelBPSSelectedModels(result, account)
}

func excelBPSSelectedModels(catalog *OpenAIModelsResponse, account *Account) (*OpenAIModelsResponse, error) {
	var envelope struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(catalog.Body, &envelope); err != nil {
		return nil, err
	}
	kept := make([]json.RawMessage, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		var model struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &model) == nil && account.isExcelBPSUpstreamModelEnabled(model.ID) {
			kept = append(kept, raw)
		}
	}
	envelope.Data = kept
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return &OpenAIModelsResponse{Body: body, ETag: codexModelsManifestBodyETag(body)}, nil
}
