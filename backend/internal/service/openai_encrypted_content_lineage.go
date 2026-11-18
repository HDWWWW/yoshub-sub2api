package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// invalid_encrypted_content 失效密文 lineage。
//
// 上游判定某轮请求中的加密 reasoning/compaction 项不可解后，同一失效密文会随
// 客户端维护的会话历史在后续每一轮重新出现，重复触发"整包被拒→剥离→重试/
// 重连"。这里按会话记录已被上游拒绝过的 encrypted_content 摘要
// （OpenAIWSStateStore，带 TTL 与容量自保护）；后续请求进场时仅剥离摘要命中
// 的项，新生成的密文摘要不同，不会被误删。

const openAIWSFallbackReasonInvalidEncryptedContent = "invalid_encrypted_content"

// openAIWSIngressSessionHashContextKey 在 gin context 中携带 ingress 会话哈希，
// 供 HTTP bridge turn 内的 lineage 记录复用同一会话键。
const openAIWSIngressSessionHashContextKey = "openai_ws_ingress_session_hash"

func openAIEncryptedContentDigest(encrypted string) string {
	sum := sha256.Sum256([]byte(encrypted))
	return hex.EncodeToString(sum[:])
}

// A request-level error is not proof that a message or an opaque content part
// is bad. Ordinary OpenAI recovery is limited to the v0.2.22 item types.
type openAIEncryptedHistoryScope uint8

const (
	openAIReasoningHistory openAIEncryptedHistoryScope = iota
	openAIExtendedHistory
)

func (scope openAIEncryptedHistoryScope) visitItemDigests(item gjson.Result, visit func(string) bool) bool {
	kind := openAIEncryptedInputItemKind(item.Get("type").String(), item.Get("role").String())
	switch kind {
	case "message":
		if scope == openAIReasoningHistory {
			return true
		}
		fallthrough
	case "reasoning", "compaction", "compaction_summary":
		encrypted := item.Get("encrypted_content")
		if encrypted.Type == gjson.String && encrypted.String() != "" && !visit(openAIEncryptedContentDigest(encrypted.String())) {
			return false
		}
	}
	if scope == openAIReasoningHistory || kind == "compaction" || kind == "compaction_summary" {
		return true
	}
	done := true
	item.Get("content").ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() != "encrypted_content" {
			return true
		}
		digest := openAIEncryptedPartDigestRaw(part)
		if digest != "" && !visit(digest) {
			done = false
			return false
		}
		return true
	})
	return done
}

// Known encrypted_content fields share the top-level digest. Unknown encrypted
// fragment envelopes use canonical JSON, so lineage also covers opaque payloads
// without guessing their field names. Neither representation stores plaintext.
func openAIEncryptedPartDigestMap(part map[string]any) string {
	if encrypted, ok := part["encrypted_content"].(string); ok && encrypted != "" {
		return openAIEncryptedContentDigest(encrypted)
	}
	encoded, err := marshalOpenAIUpstreamJSON(part)
	if err != nil {
		return ""
	}
	return openAIEncryptedContentDigest("part:" + string(encoded))
}

func openAIEncryptedPartDigestRaw(part gjson.Result) string {
	encrypted := part.Get("encrypted_content")
	if encrypted.Type == gjson.String && encrypted.String() != "" {
		return openAIEncryptedContentDigest(encrypted.String())
	}
	var decoded map[string]any
	if decodeOpenAIJSONUseNumber([]byte(part.Raw), &decoded) != nil {
		return ""
	}
	return openAIEncryptedPartDigestMap(decoded)
}

func (scope openAIEncryptedHistoryScope) itemHasInvalidDigest(item gjson.Result, invalid map[string]struct{}) bool {
	hit := false
	scope.visitItemDigests(item, func(digest string) bool {
		_, hit = invalid[digest]
		return !hit
	})
	return hit
}

// collectOpenAIEncryptedContentDigestsRaw 收集 input 中 lineage 覆盖类型所携
// 密文的摘要。payload 须遵守 replay 所有权不变式（零拷贝视图解析）。
func collectOpenAIEncryptedContentDigestsRaw(payload []byte) []string {
	return openAIExtendedHistory.collectDigests(payload)
}

func (scope openAIEncryptedHistoryScope) collectDigests(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	input := gjson.Get(openAIWSPayloadStringView(payload), "input")
	if !input.Exists() {
		return nil
	}
	var digests []string
	appendItem := func(item gjson.Result) {
		scope.visitItemDigests(item, func(digest string) bool { digests = append(digests, digest); return true })
	}

	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			appendItem(item)
			return true
		})
		return digests
	}
	if input.IsObject() {
		appendItem(input)
	}
	return digests
}

// stripOpenAIInvalidEncryptedContentItems 剥离 input 中摘要命中 invalid 的密文
// 项，剥离语义与 trimOpenAIEncryptedReasoningItems 对齐：reasoning 仅剥
// encrypted_content（连带 content:null 清理与空骨架删除），compaction/
// compaction_summary 整项删除。返回被改写的项数。
func stripOpenAIInvalidEncryptedContentItems(reqBody map[string]any, invalid map[string]struct{}) int {
	if len(invalid) == 0 {
		return 0
	}
	return openAIExtendedHistory.editItems(reqBody, invalid)
}

// rawHasInvalidDigest 探测 raw payload 是否携带命中
// invalid 摘要的密文项；零命中的常态路径不做任何改写。payload 须遵守 replay
// 所有权不变式（零拷贝视图解析）。
func (scope openAIEncryptedHistoryScope) rawHasInvalidDigest(payload []byte, invalid map[string]struct{}) bool {
	if len(payload) == 0 || len(invalid) == 0 {
		return false
	}
	input := gjson.Get(openAIWSPayloadStringView(payload), "input")
	if !input.Exists() {
		return false
	}
	hit := false
	checkItem := func(item gjson.Result) bool { return scope.itemHasInvalidDigest(item, invalid) }

	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if checkItem(item) {
				hit = true
				return false
			}
			return true
		})
		return hit
	}
	if input.IsObject() {
		return checkItem(input)
	}
	return false
}

// stripOpenAIInvalidEncryptedContentFromReplayItems 对 replay 历史序列做同款
// 剥离。有改动时返回新头数组，命中项以重写后的新正文整体替换（遵守 replay
// 所有权不变式，原正文不被修改）；未命中时原样返回。
func stripOpenAIInvalidEncryptedContentFromReplayItems(items []json.RawMessage, invalid map[string]struct{}) ([]json.RawMessage, int) {
	if len(items) == 0 || len(invalid) == 0 {
		return items, 0
	}
	hit := false
	for _, item := range items {
		if openAIExtendedHistory.itemHasInvalidDigest(gjson.ParseBytes(item), invalid) {
			hit = true
			break
		}
	}

	if !hit {
		return items, 0
	}
	stripped := 0
	next := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var decoded map[string]any
		if err := decodeOpenAIJSONUseNumber(item, &decoded); err != nil {
			next = append(next, item)
			continue
		}
		nextItem, changed, keep := openAIExtendedHistory.sanitizeItem(decoded, invalid)
		if !changed {
			next = append(next, item)
			continue
		}
		stripped++
		if !keep {
			continue
		}
		rebuilt, err := marshalOpenAIUpstreamJSON(nextItem)
		if err != nil {
			next = append(next, item)
			stripped--
			continue
		}
		next = append(next, json.RawMessage(rebuilt))
	}
	if stripped == 0 {
		return items, 0
	}
	return next, stripped
}

// stripOpenAIInvalidEncryptedContentRaw 是 raw JSON 版剥离：先零成本探测，命中
// 才解码改写重编码。返回改写后的 payload 与剥离项数；未命中时原样返回。
func stripOpenAIInvalidEncryptedContentRaw(payload []byte, invalid map[string]struct{}) ([]byte, int, error) {
	return openAIExtendedHistory.stripRaw(payload, invalid)
}

func (scope openAIEncryptedHistoryScope) stripRaw(payload []byte, invalid map[string]struct{}) ([]byte, int, error) {
	if !scope.rawHasInvalidDigest(payload, invalid) {
		return payload, 0, nil
	}
	var decoded map[string]any
	if err := decodeOpenAIJSONUseNumber(payload, &decoded); err != nil {
		return payload, 0, err
	}
	stripped := scope.editItems(decoded, invalid)
	if stripped == 0 {
		return payload, 0, nil
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(decoded)
	if err != nil {
		return payload, 0, err
	}
	return rebuilt, stripped, nil
}

// markOpenAIWSInvalidEncryptedContentLineage 把本次被上游拒绝的密文摘要写入
// 会话 lineage。digests 须在剥离前收集。
func (s *OpenAIGatewayService) markOpenAIWSInvalidEncryptedContentLineage(groupID int64, sessionHash string, digests []string) {
	if s == nil || len(digests) == 0 || strings.TrimSpace(sessionHash) == "" {
		return
	}
	stateStore := s.getOpenAIWSStateStore()
	if stateStore == nil {
		return
	}
	stateStore.MarkSessionInvalidEncryptedContent(groupID, sessionHash, digests, s.openAIWSSessionStickyTTL())
}

// sessionInvalidEncryptedContentDigests 返回会话已知失效密文摘要；全局无记录
// 时（常态）零成本返回 nil。
func (s *OpenAIGatewayService) sessionInvalidEncryptedContentDigests(groupID int64, sessionHash string) map[string]struct{} {
	if s == nil || strings.TrimSpace(sessionHash) == "" {
		return nil
	}
	stateStore := s.getOpenAIWSStateStore()
	if stateStore == nil || !stateStore.HasAnySessionInvalidEncryptedContent() {
		return nil
	}
	return stateStore.GetSessionInvalidEncryptedContentDigests(groupID, sessionHash)
}

// openAIWSLineageSessionHashFromContext 取 lineage 会话键：优先 ingress 循环
// 写入的会话哈希（与读取侧同键），否则按请求体派生。
func (s *OpenAIGatewayService) openAIWSLineageSessionHashFromContext(c *gin.Context, body []byte) string {
	if c != nil {
		if fromCtx := strings.TrimSpace(c.GetString(openAIWSIngressSessionHashContextKey)); fromCtx != "" {
			return fromCtx
		}
	}
	return s.GenerateSessionHash(c, body)
}

// markOpenAIWSInvalidEncryptedContentLineageFromPayload 在上游以
// invalid_encrypted_content 拒绝 payload 时记录其密文摘要并输出观测日志。
func (s *OpenAIGatewayService) markOpenAIWSInvalidEncryptedContentLineageFromPayload(
	c *gin.Context,
	payload []byte,
	logKey string,
	accountID int64,
	turn int,
) {
	digests := collectOpenAIEncryptedContentDigestsRaw(payload)
	if len(digests) == 0 {
		return
	}
	s.markOpenAIWSInvalidEncryptedContentLineage(
		getOpenAIGroupIDFromContext(c),
		s.openAIWSLineageSessionHashFromContext(c, payload),
		digests,
	)
	logOpenAIWSModeInfo("%s account_id=%d turn=%d digests=%d", logKey, accountID, turn, len(digests))
}

// stripSessionInvalidEncryptedContentLogged 对 payload 执行会话失效密文剥离并
// 输出观测日志（logKey / logKey+"_skip"），返回（可能已替换的）payload 与剥离
// 项数；未命中或剥离失败时原样返回。
func (scope openAIEncryptedHistoryScope) stripSessionInvalidEncryptedContentLogged(
	payload []byte,
	invalid map[string]struct{},
	logKey string,
	accountID int64,
	turn int,
) ([]byte, int) {
	strippedPayload, strippedCount, stripErr := scope.stripRaw(payload, invalid)
	if stripErr != nil {
		logOpenAIWSModeInfo(
			"%s_skip account_id=%d turn=%d reason=strip_error cause=%s",
			logKey,
			accountID,
			turn,
			truncateOpenAIWSLogValue(stripErr.Error(), openAIWSLogValueMaxLen),
		)
		return payload, 0
	}
	if strippedCount > 0 {
		logOpenAIWSModeInfo(
			"%s account_id=%d turn=%d stripped_items=%d",
			logKey,
			accountID,
			turn,
			strippedCount,
		)
	}
	return strippedPayload, strippedCount
}
