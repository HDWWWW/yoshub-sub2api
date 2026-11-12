package service

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The historical cyber names are shared by cyber_policy and bio_policy.
// Keep the provider's exact policy code throughout forwarding and recording.
const (
	OpenAIPolicyCyber = "cyber_policy"
	OpenAIPolicyBio   = "bio_policy"
)

// opsCyberPolicyKey 在 gin context 中携带上游安全策略命中标记。
// 由 gateway 服务层在检测到明确的上游安全策略错误码时设置，
// handler 在 Forward 返回后读取以触发风控记录、邮件与按上游 token 记录的用量行。
const opsCyberPolicyKey = "ops_cyber_policy"

// errOpenAICyberPolicyForwarded 表示上游安全策略错误已按当前端点格式透传给客户端
// （error 已写出/下发）。compat 路径 ForwardAsChatCompletions / ForwardAsAnthropic 出口
// 据此丢弃 result 并返回该哨兵，由 handler 按标记用量统一记账，
// 不 failover、不重复计费或写响应。
var errOpenAICyberPolicyForwarded = errors.New("openai safety policy forwarded to client")

// CyberPolicyMark records a cyber_policy or bio_policy rejection.
type CyberPolicyMark struct {
	Code           string // Original provider policy code.
	Message        string // 上游 error.message
	Body           string // Complete rejecting provider event; storage paths redact independently.
	UpstreamStatus int    // 原始上游 HTTP 状态，可与客户端状态不同。
	UpstreamInTok  int    // 上游已报 input tokens（如有）
	UpstreamOutTok int    // 上游已报 output tokens（如有）
}

func normalizeOpenAISafetyPolicyCode(code string) string {
	switch code = strings.ToLower(strings.TrimSpace(code)); code {
	case OpenAIPolicyCyber, OpenAIPolicyBio:
		return code
	default:
		return ""
	}
}

// PolicyCode retains the historical default for callers that omit Code.
func (m CyberPolicyMark) PolicyCode() string {
	if m.Code == "" {
		return OpenAIPolicyCyber
	}
	return normalizeOpenAISafetyPolicyCode(m.Code)
}

func openAISafetyPolicyMessage(code string) string {
	if code == OpenAIPolicyBio {
		return "Request blocked by upstream biological-safety policy"
	}
	return "Request blocked by upstream cyber-security policy"
}

func openAISafetyPolicyHTTPStatus(status int) int {
	if status == 403 {
		return status
	}
	return 400
}

// MarkOpsCyberPolicy 记录 cyber 标记；首个写入生效，后续忽略（同一 turn 只记一次）。
// WS 多轮场景由 handler 在每个 turn 结束后调用 ClearOpsCyberPolicy 重置。
func MarkOpsCyberPolicy(c *gin.Context, mark CyberPolicyMark) {
	if c == nil {
		return
	}
	if GetOpsCyberPolicy(c) != nil {
		return
	}
	mark.Code = mark.PolicyCode()
	if mark.Code == "" {
		return
	}
	mark.Message = strings.TrimSpace(mark.Message)
	mark.Body = strings.TrimSpace(mark.Body)
	c.Set(opsCyberPolicyKey, &mark)
}

// GetOpsCyberPolicy 返回 cyber 标记，未命中（或已被 Clear）返回 nil。
func GetOpsCyberPolicy(c *gin.Context) *CyberPolicyMark {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(opsCyberPolicyKey); ok {
		if m, ok := v.(*CyberPolicyMark); ok && m != nil {
			return m
		}
	}
	return nil
}

// ClearOpsCyberPolicy 清除 cyber 标记（typed-nil 覆盖；gin context 无并发安全的
// 删除原语，Set 走内部锁，与异步 GetOpsCyberPolicy 不构成 data race）。
// 仅 WS 多轮路径在 turn 收尾调用；HTTP 单请求路径不调用（context 随请求销毁，
// 且中间件 shouldSkipOpsErrorLogForCyber 依赖标记防双写）。
// WS 路径 clear 发生在中间件收尾之前，连接响应状态为 101，不触发中间件 status>=400
// 落库分支，故无双写/漏写。
func ClearOpsCyberPolicy(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(opsCyberPolicyKey, (*CyberPolicyMark)(nil))
}

// detectOpenAICyberPolicy recognizes explicit provider policy codes only.
// Message text and access_programs metadata never imply a policy rejection.
func detectOpenAICyberPolicy(payload []byte) (bool, string, string) {
	for _, path := range []string{"error", "response.error"} {
		err := gjson.GetBytes(payload, path)
		for _, field := range []string{"code", "type"} {
			if code := normalizeOpenAISafetyPolicyCode(err.Get(field).String()); code != "" {
				return true, code, strings.TrimSpace(err.Get("message").String())
			}
		}
	}
	return false, "", ""
}

// Policy codes override retryable statuses and rate-limit wrappers. Ordinary
// moderation is not recorded as cyber/bio and does not change user ban rules.
func isOpenAIContentPolicyRejection(payload []byte) bool {
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		if isOpenAIContentPolicyCode(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func isOpenAIContentPolicyCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case OpenAIPolicyCyber, OpenAIPolicyBio, "content_policy", "content_policy_violation",
		"content_filter", "content_filter_error", "safety_policy_violation", "safety_violation",
		"safety_error", "moderation_blocked":
		return true
	}
	return false
}

// Keep the provider policy code visible to gateways that read only error.code.
// A contradictory outer quota wrapper must not mask response.error's refusal.
func openAIPolicyRejectionPayload(payload []byte) []byte {
	for _, path := range []string{"error", "response.error"} {
		err := gjson.GetBytes(payload, path)
		code := err.Get("code").String()
		if !isOpenAIContentPolicyCode(code) {
			code = err.Get("type").String()
		}
		if !isOpenAIContentPolicyCode(code) {
			continue
		}
		if !isOpenAIContentPolicyCode(err.Get("code").String()) {
			payload, _ = sjson.SetBytes(payload, path+".code", code)
			err = gjson.GetBytes(payload, path)
		}
		if path == "response.error" && gjson.GetBytes(payload, "error").IsObject() {
			payload, _ = sjson.SetRawBytes(payload, "error", []byte(err.Raw))
		}
		return payload
	}
	return payload
}

// HTTP clients expect error.code even when the upstream used response.failed.
// Retain the complete response and usage alongside the standard error object.
func openAIPolicyHTTPErrorPayload(payload []byte) []byte {
	payload = openAIPolicyRejectionPayload(payload)
	if isOpenAIContentPolicyRejection(payload) && !gjson.GetBytes(payload, "error").IsObject() {
		if nested := gjson.GetBytes(payload, "response.error"); nested.IsObject() {
			payload, _ = sjson.SetRawBytes(payload, "error", []byte(nested.Raw))
		}
	}
	return payload
}

func markOpenAICyberPolicyEvent(c *gin.Context, payload []byte, upstreamStatus int, usage *OpenAIUsage) bool {
	hit, code, message := detectOpenAICyberPolicy(payload)
	if !hit {
		return false
	}
	mark := CyberPolicyMark{
		Code:           code,
		Message:        message,
		Body:           string(payload),
		UpstreamStatus: upstreamStatus,
	}
	if usage != nil {
		mark.UpstreamInTok = usage.InputTokens
		mark.UpstreamOutTok = usage.OutputTokens
	}
	MarkOpsCyberPolicy(c, mark)
	return true
}
