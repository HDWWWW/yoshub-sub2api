package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	pf "github.com/Wei-Shaw/sub2api/internal/pkg/codexpromptfilter"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const ContentModerationRuleVersion = "codex2api-b6a827f9357a6cf1ac4c211854726d808406c44f"

// Only the local rule layer is configurable here. Model review and account
// penalties belong to the existing Sub2 services, never to this adapter.
type ContentModerationRuleFilter struct {
	Enabled               bool               `json:"enabled"`
	Mode                  string             `json:"mode"`
	Threshold             int                `json:"threshold"`
	StrictThreshold       int                `json:"strict_threshold"`
	StrictTerminalEnabled bool               `json:"strict_terminal_enabled"`
	CustomPatterns        []pf.PatternConfig `json:"custom_patterns"`
	DisabledPatterns      []string           `json:"disabled_patterns"`
}

func DefaultContentModerationRuleFilter() *ContentModerationRuleFilter {
	cfg := pf.RecommendedConfig()
	return &ContentModerationRuleFilter{Mode: cfg.Mode, Threshold: cfg.Threshold, StrictThreshold: cfg.StrictThreshold, StrictTerminalEnabled: cfg.StrictTerminalEnabled, CustomPatterns: []pf.PatternConfig{}, DisabledPatterns: []string{}}
}

func cloneRuleFilter(in *ContentModerationRuleFilter) *ContentModerationRuleFilter {
	if in == nil {
		return DefaultContentModerationRuleFilter()
	}
	raw, _ := json.Marshal(in)
	var out ContentModerationRuleFilter
	_ = json.Unmarshal(raw, &out)
	if out.CustomPatterns == nil {
		out.CustomPatterns = []pf.PatternConfig{}
	}
	if out.DisabledPatterns == nil {
		out.DisabledPatterns = []string{}
	}
	return &out
}

func (cfg *ContentModerationRuleFilter) upstream() pf.Config {
	out := pf.RecommendedConfig()
	if cfg == nil {
		return out
	}
	out.Enabled, out.Mode = cfg.Enabled, cfg.Mode
	out.Threshold, out.StrictThreshold = cfg.Threshold, cfg.StrictThreshold
	out.StrictTerminalEnabled = cfg.StrictTerminalEnabled
	out.CustomPatterns, out.DisabledPatterns = cfg.CustomPatterns, cfg.DisabledPatterns
	return pf.NormalizeConfig(out)
}

func validateRuleFilter(cfg *ContentModerationRuleFilter) error {
	invalid := func(msg string) error { return infraerrors.BadRequest("INVALID_RULE_FILTER", msg) }
	if cfg == nil {
		return invalid("规则配置不能为空")
	}
	if cfg.Mode != pf.ModeBlock && cfg.Mode != pf.ModeMonitor && cfg.Mode != pf.ModeWarn {
		return invalid("规则模式必须为 block、monitor 或 warn")
	}
	if cfg.Threshold < 1 || cfg.Threshold > 500 || cfg.StrictThreshold < cfg.Threshold || cfg.StrictThreshold > 1000 {
		return invalid("普通阈值须为 1–500，严格阈值须不小于普通阈值且不超过 1000")
	}
	if len(cfg.CustomPatterns) > 100 || len(cfg.DisabledPatterns) > 230 {
		return invalid("最多 100 条自定义规则")
	}
	raw, _ := json.Marshal(cfg)
	if len(raw) > 1024*1024 {
		return invalid("规则配置不能超过 1 MiB")
	}
	names := map[string]bool{}
	exactNames := map[string]bool{}
	for _, r := range pf.BuiltinPatternConfigs() {
		names[strings.ToLower(r.Name)] = true
		exactNames[r.Name] = true
	}
	for _, r := range cfg.CustomPatterns {
		name := strings.TrimSpace(r.Name)
		if name != r.Name || names[strings.ToLower(name)] {
			return invalid("规则名称重复或含首尾空格: " + r.Name)
		}
		if len(r.Category) > 100 {
			return invalid("规则分类不能超过 100 字节")
		}
		// The upstream admission check does not include authorization exclusions
		// when that deployment option is off. Validate their syntax as well.
		if len(r.AuthorizationExcludePatterns) > 64 {
			return invalid("授权排除条件最多 64 条")
		}
		for _, pattern := range r.AuthorizationExcludePatterns {
			if utf8.RuneCountInString(pattern) > 5000 {
				return invalid("正则不能超过 5000 字符")
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return invalid(err.Error())
			}
		}
		if issue := pf.AuditPatternConfig(r); issue != nil {
			return invalid(fmt.Sprintf("%s: %s", r.Name, issue.Message))
		}
		names[strings.ToLower(name)] = true
		exactNames[name] = true
	}
	for _, name := range cfg.DisabledPatterns {
		if !exactNames[name] {
			return invalid("未知的禁用规则: " + name)
		}
	}
	return nil
}

func ContentModerationRuleCatalog() any {
	return struct {
		Version  string                       `json:"version"`
		Presets  []pf.PatternConfig           `json:"presets"`
		Defaults *ContentModerationRuleFilter `json:"defaults"`
	}{ContentModerationRuleVersion, pf.BuiltinPatternConfigs(), DefaultContentModerationRuleFilter()}
}

type ContentModerationRuleSignal struct {
	Origin    string     `json:"origin"`
	LayerMode string     `json:"layer_mode"`
	Score     int        `json:"score"`
	RawScore  int        `json:"raw_score"`
	Action    string     `json:"action"`
	Matches   []pf.Match `json:"matches"`
	Context   string     `json:"context"`
	Keywords  []string   `json:"keywords"`
}

type ContentModerationRuleEvidence struct {
	Version         string                        `json:"version"`
	Action          string                        `json:"action"`
	WouldAction     string                        `json:"would_action"`
	Score           int                           `json:"score"`
	RawScore        int                           `json:"raw_score"`
	AuditScore      int                           `json:"audit_score"`
	Threshold       int                           `json:"threshold"`
	StrictThreshold int                           `json:"strict_threshold"`
	Reason          string                        `json:"reason"`
	Truncated       bool                          `json:"truncated"`
	Deferred        bool                          `json:"deferred"`
	Signals         []ContentModerationRuleSignal `json:"signals"`
}

var ruleGuardPipeline = pf.NewGuardPipeline()
var ruleGuardShadowSlots = make(chan struct{}, 2)
var ruleHitMarker = regexp.MustCompile(`(?s)⟦PF_HIT⟧(.*?)⟦/PF_HIT⟧`)

func ruleEvidence(d pf.Decision, cfg *ContentModerationRuleFilter, limit int) *ContentModerationRuleEvidence {
	e := &ContentModerationRuleEvidence{Version: ContentModerationRuleVersion, Action: d.Action, WouldAction: d.WouldAction, Score: d.Score, RawScore: d.RawScore, AuditScore: d.AuditScore, Threshold: cfg.Threshold, StrictThreshold: cfg.StrictThreshold, Reason: d.Reason, Truncated: d.Truncated, Signals: []ContentModerationRuleSignal{}}
	remaining := normalizeModerationKeywordContextChars(limit)
	for _, sig := range d.Signals {
		marked := pf.SignalMatchContext(sig)
		words := []string{}
		for _, m := range ruleHitMarker.FindAllStringSubmatch(marked, -1) {
			if m[1] != "" && redactContentModerationSecrets(m[1]) == m[1] {
				words = append(words, trimRunes(m[1], 200))
			}
		}
		plain := strings.ReplaceAll(strings.ReplaceAll(marked, pf.HitStartMarker, ""), pf.HitEndMarker, "")
		text := trimRunes(redactContentModerationSecrets(plain), remaining)
		if len(words) == 0 {
			words = pf.SignalContextKeywords(sig, cfg.upstream(), text)
		}
		remaining -= utf8.RuneCountInString(text)
		e.Signals = append(e.Signals, ContentModerationRuleSignal{Origin: string(sig.Origin), LayerMode: sig.LayerMode, Score: sig.Score, RawScore: sig.RawScore, Action: sig.SuggestedAction, Matches: append([]pf.Match(nil), sig.Matches...), Context: text, Keywords: words})
	}
	return e
}

func buildRuleEnvelope(input ContentModerationCheckInput, cfg pf.Config) pf.RequestEnvelope {
	endpoint, body := input.Endpoint, input.Body
	switch input.Protocol {
	case ContentModerationProtocolOpenAIResponses:
		endpoint = "/v1/responses"
	case ContentModerationProtocolOpenAIChat:
		endpoint = "/v1/chat/completions"
	case ContentModerationProtocolAnthropicMessages:
		endpoint = "/v1/messages"
	case ContentModerationProtocolOpenAIImages:
		endpoint = "/v1/images/generations"
	case ContentModerationProtocolTypeSafeSystemOne:
		return pf.BuildTextEnvelopeWithModelsAndConfig(extractContentModerationKeywordText(input.Protocol, body), "/v1/responses", input.Model, input.Model, pf.TransportHTTP, cfg)
	case ContentModerationProtocolGemini:
		// Map parts individually so a functionResponse never inherits user trust.
		messages := []any{}
		for _, item := range gjson.GetBytes(body, "contents").Array() {
			role := item.Get("role").String()
			if role == "model" {
				role = "assistant"
			}
			if role == "" {
				role = "user"
			}
			parts := []any{}
			for _, part := range item.Get("parts").Array() {
				if value := part.Get("text"); value.Type == gjson.String {
					parts = append(parts, map[string]any{"type": "text", "text": value.String()})
				}
				for _, key := range []string{"functionResponse", "function_response"} {
					if v := part.Get(key); v.Exists() {
						parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": v.Get("name").String(), "content": v.Get("response").Raw})
					}
				}
				for _, key := range []string{"functionCall", "function_call"} {
					if v := part.Get(key); v.Exists() {
						parts = append(parts, map[string]any{"type": "tool_use", "name": v.Get("name").String(), "input": v.Get("args").Value()})
					}
				}
			}
			messages = append(messages, map[string]any{"role": role, "content": parts})
		}
		body, _ = json.Marshal(map[string]any{"messages": messages})
		endpoint = "/v1/messages"
	}
	return pf.BuildEnvelopeWithModelsAndConfig(body, endpoint, input.Model, input.Model, pf.TransportHTTP, cfg)
}

// Preview uses exactly the production pipeline and performs no writes or calls
// to an external model. Optional raw bodies exercise protocol/source handling.
func TestContentModerationRuleFilter(ctx context.Context, cfg *ContentModerationRuleFilter, text string, body json.RawMessage, protocol string) (*ContentModerationRuleEvidence, error) {
	if err := validateRuleFilter(cfg); err != nil {
		return nil, err
	}
	if len(text)+len(body) > 1024*1024 {
		return nil, infraerrors.BadRequest("RULE_TEST_TOO_LARGE", "测试内容不能超过 1 MiB")
	}
	if len(body) == 0 {
		body, _ = json.Marshal(map[string]string{"input": text})
		protocol = ContentModerationProtocolOpenAIResponses
	}
	if !json.Valid(body) {
		return nil, infraerrors.BadRequest("INVALID_RULE_TEST_BODY", "请求正文必须为有效 JSON")
	}
	upstream := cfg.upstream()
	d := ruleGuardPipeline.Evaluate(ctx, pf.GuardRequest{Envelope: buildRuleEnvelope(ContentModerationCheckInput{Body: body, Protocol: protocol}, upstream), Config: upstream})
	e := ruleEvidence(d, cfg, defaultModerationKeywordContextChars)
	if audit, ok := d.DeferredAudit(); ok {
		shadow := ruleGuardPipeline.EvaluateDeferred(ctx, audit)
		e.Signals = append(e.Signals, ruleEvidence(shadow, cfg, defaultModerationKeywordContextChars).Signals...)
		e.AuditScore = max(e.AuditScore, shadow.AuditScore)
	}
	return e, nil
}

func (s *ContentModerationService) checkRuleFilter(ctx context.Context, input ContentModerationCheckInput, snapshot *contentModerationRuntimeSnapshot) *ContentModerationDecision {
	cfg := snapshot.config
	if cfg.RuleFilter == nil || !cfg.RuleFilter.Enabled {
		return nil
	}
	upstream := cfg.RuleFilter.upstream()
	d := ruleGuardPipeline.Evaluate(ctx, pf.GuardRequest{Envelope: buildRuleEnvelope(input, upstream), Config: upstream})
	blocking := d.Action == pf.ActionBlock && cfg.Mode == ContentModerationModePreBlock && !input.riskControlLogOnly
	if len(d.Signals) > 0 {
		action := ContentModerationActionRuleObserve
		if blocking {
			action = ContentModerationActionRuleBlock
		}
		log := s.buildLog(input, cfg, action, true, "local_rule", 0, nil, "", nil, nil, "")
		log.ThresholdSnapshot = nil
		log.InputEvidence.RuleFilter = ruleEvidence(d, cfg.RuleFilter, cfg.KeywordContextChars)
		s.enqueueRecord(input, cfg, log, "", false, false)
	}
	if audit, ok := d.DeferredAudit(); ok {
		s.enqueueRuleShadow(input, cfg, audit)
	}
	if !blocking {
		return nil
	}
	s.recordPreBlockSyncMetric(0, ContentModerationActionRuleBlock)
	return &ContentModerationDecision{Allowed: false, Blocked: true, Flagged: true, Action: ContentModerationActionRuleBlock, HighestCategory: "local_rule", StatusCode: cfg.BlockStatus, Message: cfg.BlockMessage}
}

type contentModerationRuleAudit struct{ audit pf.DeferredAudit }

func (s *ContentModerationService) enqueueRuleShadow(input ContentModerationCheckInput, cfg *ContentModerationConfig, audit pf.DeferredAudit) {
	if s.asyncQueue == nil {
		return
	}
	if len(s.asyncQueue) >= min(cfg.QueueSize, 256) {
		s.asyncDropped.Add(1)
		return
	}
	log := s.buildLog(input, cfg, ContentModerationActionRuleObserve, true, "local_rule", 0, nil, "", nil, nil, "")
	log.ThresholdSnapshot = nil
	task := contentModerationTask{ruleAudit: &contentModerationRuleAudit{audit: audit}, log: log, config: cloneContentModerationConfig(cfg), enqueuedAt: time.Now()}
	select {
	case s.asyncQueue <- task:
		s.asyncEnqueued.Add(1)
	default:
		s.asyncDropped.Add(1)
	}
}

func (s *ContentModerationService) processRuleShadow(ctx context.Context, task contentModerationTask, current *contentModerationRuntimeSnapshot) {
	if !current.riskControlEnabled || !current.config.Enabled || current.config.Mode == ContentModerationModeOff || current.config.RuleFilter == nil || !current.config.RuleFilter.Enabled {
		return
	}
	select {
	case ruleGuardShadowSlots <- struct{}{}:
		defer func() { <-ruleGuardShadowSlots }()
	default:
		s.asyncDropped.Add(1)
		return
	}
	s.asyncActive.Add(1)
	defer s.asyncActive.Add(-1)
	d := ruleGuardPipeline.EvaluateDeferred(ctx, task.ruleAudit.audit)
	if ctx.Err() != nil || len(d.Signals) == 0 {
		return
	}
	e := ruleEvidence(d, task.config.RuleFilter, task.config.KeywordContextChars)
	e.Action = pf.ActionAllow
	e.Deferred = true
	task.log.InputEvidence.RuleFilter = e
	delay := int(time.Since(task.enqueuedAt).Milliseconds())
	task.log.QueueDelayMS = &delay
	s.persistContentModerationLog(ctx, task.config, task.log, "", false, false)
	s.asyncProcessed.Add(1)
}
