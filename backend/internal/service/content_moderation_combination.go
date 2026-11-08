package service

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	ContentModerationActionRuleBlock   = "rule_block"
	ContentModerationActionRuleObserve = "rule_observe"
	maxCombinationRules                = 100
)

// Groups are ANDed; keywords within a group are ORed. No topic is built into
// the matcher. Rules live in the shared moderation settings, not in code.
type ContentModerationCombinationRule struct {
	ID               string                              `json:"id"`
	Name             string                              `json:"name"`
	Enabled          bool                                `json:"enabled"`
	Action           string                              `json:"action"` // block | observe
	Scope            string                              `json:"scope"`  // conversation | current_user
	WindowCharacters int                                 `json:"window_characters"`
	Groups           []ContentModerationCombinationGroup `json:"groups"`
}

type ContentModerationCombinationGroup struct {
	Name     string   `json:"name"`
	Keywords []string `json:"keywords"`
}

type ContentModerationCombinationHit struct {
	RuleID           string                                    `json:"rule_id"`
	RuleName         string                                    `json:"rule_name"`
	Action           string                                    `json:"action"`
	Scope            string                                    `json:"scope"`
	WindowCharacters int                                       `json:"window_characters"`
	Source           string                                    `json:"source"`
	Matches          []ContentModerationCombinationMatchedWord `json:"matches"`
	Context          string                                    `json:"context"`
}

type ContentModerationCombinationMatchedWord struct {
	Group   string `json:"group"`
	Keyword string `json:"keyword"`
}

func cloneCombinationRules(in []ContentModerationCombinationRule) []ContentModerationCombinationRule {
	out := make([]ContentModerationCombinationRule, len(in))
	for i, rule := range in {
		out[i] = rule
		out[i].Groups = make([]ContentModerationCombinationGroup, len(rule.Groups))
		for j, group := range rule.Groups {
			out[i].Groups[j] = ContentModerationCombinationGroup{Name: group.Name, Keywords: append([]string(nil), group.Keywords...)}
		}
	}
	return out
}

func normalizeCombinationRules(in []ContentModerationCombinationRule) ([]ContentModerationCombinationRule, error) {
	invalid := func(message string) ([]ContentModerationCombinationRule, error) {
		return nil, infraerrors.BadRequest("INVALID_COMBINATION_RULES", message)
	}
	if len(in) > maxCombinationRules {
		return invalid("组合规则最多 100 条")
	}
	out := cloneCombinationRules(in)
	ids := make(map[string]bool, len(in))
	totalWords, totalCharacters := 0, 0
	for i := range out {
		rule := &out[i]
		rule.ID, rule.Name = strings.TrimSpace(rule.ID), strings.TrimSpace(rule.Name)
		if rule.ID == "" || len(rule.ID) > 64 || ids[rule.ID] {
			return invalid("组合规则 ID 必须唯一且为 1–64 位字母、数字、下划线或连字符")
		}
		for _, c := range rule.ID {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return invalid("组合规则 ID 只能包含字母、数字、下划线或连字符")
			}
		}
		ids[rule.ID] = true
		if rule.Name == "" || utf8.RuneCountInString(rule.Name) > 120 {
			return invalid("组合规则名称必须为 1–120 字符")
		}
		if (rule.Action != "block" && rule.Action != "observe") || (rule.Scope != "conversation" && rule.Scope != "current_user") {
			return invalid(fmt.Sprintf("规则 %s 的处理方式或检查范围无效", rule.Name))
		}
		if rule.WindowCharacters < 1 || rule.WindowCharacters > 12000 || len(rule.Groups) < 2 || len(rule.Groups) > 8 {
			return invalid(fmt.Sprintf("规则 %s 需要 2–8 个词组，匹配距离为 1–12000 字符", rule.Name))
		}
		for j := range rule.Groups {
			group := &rule.Groups[j]
			group.Name = strings.TrimSpace(group.Name)
			if group.Name == "" || utf8.RuneCountInString(group.Name) > 80 || len(group.Keywords) > 200 {
				return invalid(fmt.Sprintf("规则 %s 的词组名称须为 1–80 字符，每组最多 200 个词", rule.Name))
			}
			words := make([]string, 0, len(group.Keywords))
			seen := map[string]bool{}
			for _, word := range group.Keywords {
				word = strings.TrimSpace(word)
				if word == "" || seen[strings.ToLower(word)] {
					continue
				}
				if utf8.RuneCountInString(word) > 200 {
					return invalid("组合规则中的单个关键词不能超过 200 字符")
				}
				seen[strings.ToLower(word)] = true
				words = append(words, word)
				totalWords++
				totalCharacters += utf8.RuneCountInString(word)
			}
			if len(words) == 0 {
				return invalid(fmt.Sprintf("规则 %s 的每个词组至少需要一个关键词", rule.Name))
			}
			group.Keywords = words
		}
	}
	if totalWords > 5000 || totalCharacters > 100000 {
		return invalid("组合规则合计最多 5000 个关键词、100000 字符")
	}
	return out, nil
}

type combinationTerm struct {
	rule, group, length int
	word                string
	leftWord, rightWord bool
}

type combinationNode struct {
	next    map[rune]int
	failure int
	output  []combinationTerm
}

type contentModerationCombinationMatcher struct {
	rules []ContentModerationCombinationRule
	nodes []combinationNode
}

func newCombinationMatcher(rules []ContentModerationCombinationRule) *contentModerationCombinationMatcher {
	m := &contentModerationCombinationMatcher{rules: rules, nodes: []combinationNode{{next: map[rune]int{}}}}
	for ri, rule := range rules {
		if !rule.Enabled {
			continue
		}
		for gi, group := range rule.Groups {
			for _, word := range group.Keywords {
				chars := []rune(strings.ToLower(word))
				if len(chars) == 0 {
					continue
				}
				state := 0
				for _, c := range chars {
					next, ok := m.nodes[state].next[c]
					if !ok {
						next = len(m.nodes)
						m.nodes[state].next[c] = next
						m.nodes = append(m.nodes, combinationNode{next: map[rune]int{}})
					}
					state = next
				}
				m.nodes[state].output = append(m.nodes[state].output, combinationTerm{ri, gi, len(chars), word, combinationASCIIWord(chars[0]), combinationASCIIWord(chars[len(chars)-1])})
			}
		}
	}
	if len(m.nodes) == 1 {
		return nil
	}
	queue := []int{}
	for _, node := range m.nodes[0].next {
		queue = append(queue, node)
	}
	for index := 0; index < len(queue); index++ {
		state := queue[index]
		for char, next := range m.nodes[state].next {
			failure := m.nodes[state].failure
			for failure != 0 && m.nodes[failure].next[char] == 0 {
				failure = m.nodes[failure].failure
			}
			m.nodes[next].failure = m.nodes[failure].next[char]
			m.nodes[next].output = append(m.nodes[next].output, m.nodes[m.nodes[next].failure].output...)
			queue = append(queue, next)
		}
	}
	return m
}

func combinationASCIIWord(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// The automaton scans all configured keywords together. Position and distance
// use Unicode characters, never UTF-8 bytes. Each message gets fresh state.
func (m *contentModerationCombinationMatcher) matchMessage(message combinationMessage, found map[int]ContentModerationCombinationHit) {
	if m == nil {
		return
	}
	remaining := 0
	for i, rule := range m.rules {
		_, done := found[i]
		if rule.Enabled && !done && (message.current || rule.Scope == "conversation") {
			remaining++
		}
	}
	if remaining == 0 {
		return
	}
	type occurrence struct {
		start, end int
		word       string
	}
	latest := make([][]occurrence, len(m.rules))
	for i, rule := range m.rules {
		latest[i] = make([]occurrence, len(rule.Groups))
	}
	chars := []rune(message.text)
	state := 0
	for position, original := range chars {
		c := unicode.ToLower(original)
		for state != 0 && m.nodes[state].next[c] == 0 {
			state = m.nodes[state].failure
		}
		state = m.nodes[state].next[c]
		for _, term := range m.nodes[state].output {
			if _, done := found[term.rule]; done {
				continue
			}
			rule := m.rules[term.rule]
			if rule.Scope == "current_user" && !message.current {
				continue
			}
			start, end := position+1-term.length, position+1
			if term.leftWord && start > 0 && combinationASCIIWord(chars[start-1]) || term.rightWord && end < len(chars) && combinationASCIIWord(chars[end]) {
				continue
			}
			prev := latest[term.rule][term.group]
			if prev.end > 0 && prev.start > start {
				continue
			}
			latest[term.rule][term.group] = occurrence{start, end, term.word}
			left, right, complete := start, end, true
			for _, hit := range latest[term.rule] {
				if hit.end == 0 {
					complete = false
					break
				}
				left, right = min(left, hit.start), max(right, hit.end)
			}
			if !complete || right-left > rule.WindowCharacters {
				continue
			}
			words := make([]ContentModerationCombinationMatchedWord, len(rule.Groups))
			for i, hit := range latest[term.rule] {
				words[i] = ContentModerationCombinationMatchedWord{rule.Groups[i].Name, hit.word}
			}
			found[term.rule] = ContentModerationCombinationHit{
				RuleID: rule.ID, RuleName: rule.Name, Action: rule.Action, Scope: rule.Scope,
				WindowCharacters: rule.WindowCharacters, Source: message.source, Matches: words,
				Context: string(chars[left:right]),
			}
			remaining--
			if remaining == 0 {
				return
			}
		}
	}
}

func (m *contentModerationCombinationMatcher) match(messages []combinationMessage) []ContentModerationCombinationHit {
	out := []ContentModerationCombinationHit{}
	if m == nil {
		return out
	}
	found := map[int]ContentModerationCombinationHit{}
	for _, message := range messages {
		m.matchMessage(message, found)
	}
	// Stable configured order, regardless of map iteration or keyword order.
	for i := range m.rules {
		if hit, ok := found[i]; ok {
			out = append(out, hit)
		}
	}
	return out
}

// Preview is local and has no audit, hash, email or account side effects.
func TestContentModerationCombinationRules(rules []ContentModerationCombinationRule, text string) ([]ContentModerationCombinationHit, error) {
	normalized, err := normalizeCombinationRules(rules)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(text) > 100000 {
		return nil, infraerrors.BadRequest("RULE_TEST_TOO_LARGE", "规则测试文本不能超过 100000 字符")
	}
	hits := newCombinationMatcher(normalized).match([]combinationMessage{{text: text, source: "test", current: true}})
	for i := range hits {
		hits[i].Context = redactContentModerationSecrets(hits[i].Context)
	}
	return hits, nil
}

func (s *ContentModerationService) checkCombinationRules(input ContentModerationCheckInput, snapshot *contentModerationRuntimeSnapshot) *ContentModerationDecision {
	if snapshot.combinationMatcher == nil {
		return nil
	}
	cfg := snapshot.config
	hits := snapshot.combinationMatcher.match(extractCombinationMessages(input.Protocol, input.Body))
	if len(hits) == 0 {
		return nil
	}
	blocking := false
	for _, hit := range hits {
		blocking = blocking || hit.Action == "block"
	}
	blocking = blocking && cfg.Mode == ContentModerationModePreBlock && !input.riskControlLogOnly
	action := ContentModerationActionRuleObserve
	if blocking {
		action = ContentModerationActionRuleBlock
	}
	// One combination audit event per request, including all matched rules. These local
	// topic filters never poison the hash cache or accumulate account penalties.
	log := s.buildLog(input, cfg, action, true, "combination_rule", 0, nil, "", nil, nil, "")
	log.ThresholdSnapshot = nil
	userTexts := make(map[string]string)
	for _, message := range extractModerationUserBodies(input.Protocol, input.Body) {
		userTexts[message.source] = message.text
		if message.current {
			userTexts["current_user"] = normalizeContentModerationText(message.text)
		}
	}
	for i := range hits {
		contextLog := &ContentModerationLog{MatchedKeyword: hits[i].Context}
		setContentModerationEvidence(contextLog, userTexts[hits[i].Source], cfg.KeywordContextChars)
		hits[i].Context = contextLog.InputEvidence.MatchContext
	}
	log.InputEvidence.CombinationRules = hits
	s.enqueueRecord(input, cfg, log, "", false, false)
	if !blocking {
		return nil
	}
	s.recordPreBlockSyncMetric(0, action)
	return &ContentModerationDecision{Allowed: false, Blocked: true, Flagged: true, Action: action, HighestCategory: "combination_rule", StatusCode: cfg.BlockStatus, Message: cfg.BlockMessage}
}
