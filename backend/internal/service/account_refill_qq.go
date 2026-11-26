package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var refillQQAppIDPattern = regexp.MustCompile(`^[0-9]{1,32}$`)
var refillQQOpenIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func validateRefillQQConfig(c AccountRefillNotificationConfig) error {
	if c.Channel != "" && c.Channel != "wecom" && c.Channel != "qq" {
		return errors.New("请选择企业微信或 QQ 通知渠道")
	}
	if c.Channel != "qq" {
		return nil
	}
	if !refillQQAppIDPattern.MatchString(c.QQAppID) {
		return errors.New("请填写有效的 QQ 机器人 AppID")
	}
	if c.QQTargetType != "group" && c.QQTargetType != "c2c" {
		return errors.New("请选择 QQ 群或私聊通知目标")
	}
	if !refillQQOpenIDPattern.MatchString(c.QQTargetOpenID) || strings.Trim(c.QQTargetOpenID, "0123456789") == "" {
		return errors.New("请填写当前机器人事件中的 OpenID，不是 QQ 号或群号")
	}
	return nil
}

func validateRefillQQSecret(secret string) error {
	if secret == "" || len(secret) > 256 {
		return errors.New("QQ ClientSecret 格式无效")
	}
	for _, b := range []byte(secret) {
		if b < 33 || b > 126 {
			return errors.New("QQ ClientSecret 格式无效")
		}
	}
	return nil
}

type qqRefillDeliveryError struct {
	message   string
	retryable bool
}

func (e *qqRefillDeliveryError) Error() string { return e.message }

type qqRefillReply struct {
	Code        *int            `json:"code"`
	ID          string          `json:"id"`
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

type qqRefillNotifier struct {
	client      *http.Client
	mu          sync.Mutex
	fingerprint [32]byte
	token       string
	expires     time.Time
}

func (s *AccountRefillService) qqNotificationSender() *qqRefillNotifier {
	root := s.rootService()
	root.rulesMu.Lock()
	defer root.rulesMu.Unlock()
	if root.qqNotifier == nil {
		root.qqNotifier = &qqRefillNotifier{}
	}
	return root.qqNotifier
}

func (n *qqRefillNotifier) post(ctx context.Context, path, token string, payload any, delivery bool) (*qqRefillReply, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("QQ 通知编码失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.bot.qq.com"+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("QQ 请求构建失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "QQBot "+token)
	}
	client := n.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		if delivery {
			return nil, &qqRefillDeliveryError{message: "QQ 发送结果不明，请核对接收端；未自动重发"}
		}
		return nil, &qqRefillDeliveryError{message: "QQ 访问凭证获取失败或超时", retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16385))
	var reply qqRefillReply
	if readErr != nil || len(raw) > 16384 || json.Unmarshal(raw, &reply) != nil {
		return nil, &qqRefillDeliveryError{message: "QQ 未明确确认请求结果，请核对接收端", retryable: !delivery}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (reply.Code != nil && *reply.Code != 0) {
		code, detail := 0, "请检查机器人权限、接收目标或平台状态"
		if reply.Code != nil {
			code = *reply.Code
		}
		switch code {
		case 40034105, 40054013, 304064:
			detail = "主动通知无权限或接收方拒收"
		case 40034100, 100001:
			detail = "发送频率或平台配额受限"
		case 100007, 100016, 10004:
			detail = "机器人 AppID、密钥或状态异常"
		case 40054004:
			detail = "请先添加机器人好友"
		case 40054016:
			detail = "机器人已下线"
		}
		if delivery && resp.StatusCode >= 500 {
			detail = "发送结果不明，请核对接收端；未自动重发"
		}
		return nil, &qqRefillDeliveryError{message: fmt.Sprintf("QQ 请求失败（HTTP %d，代码 %d）：%s", resp.StatusCode, code, detail), retryable: resp.StatusCode == 401 || resp.StatusCode == 429 || code == 40034100 || (!delivery && resp.StatusCode >= 500)}
	}
	return &reply, nil
}

func (n *qqRefillNotifier) accessToken(ctx context.Context, c AccountRefillNotificationConfig, secret string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := sha256.Sum256([]byte(c.QQAppID + "\x00" + secret))
	if n.fingerprint == key && n.token != "" && time.Now().Before(n.expires) {
		return n.token, nil
	}
	n.token = ""
	reply, err := n.post(ctx, "/app/getAppAccessToken", "", map[string]string{"appId": c.QQAppID, "clientSecret": secret}, false)
	if err != nil {
		return "", err
	}
	seconds, err := strconv.Atoi(strings.Trim(string(reply.ExpiresIn), `"`))
	if err != nil || seconds < 1 || seconds > 7200 || reply.AccessToken == "" || len(reply.AccessToken) > 8192 {
		return "", errors.New("QQ 访问凭证响应无效")
	}
	for _, b := range []byte(reply.AccessToken) {
		if b < 33 || b > 126 {
			return "", errors.New("QQ 访问凭证响应无效")
		}
	}
	n.fingerprint, n.token = key, reply.AccessToken
	n.expires = time.Now().Add(time.Duration(max(0, seconds-60)) * time.Second)
	return n.token, nil
}

func (n *qqRefillNotifier) Send(ctx context.Context, c AccountRefillNotificationConfig, secret, content string) error {
	if err := validateRefillQQConfig(c); err != nil {
		return err
	}
	if err := validateRefillQQSecret(secret); err != nil {
		return err
	}
	token, err := n.accessToken(ctx, c, secret)
	if err != nil {
		return err
	}
	resource := "groups"
	if c.QQTargetType == "c2c" {
		resource = "users"
	}
	reply, err := n.post(ctx, "/v2/"+resource+"/"+c.QQTargetOpenID+"/messages", token, map[string]any{"msg_type": 0, "content": content}, true)
	if err != nil {
		// A rejected/expired token is refreshed on the next notification, never
		// by replaying a message whose delivery may already have succeeded.
		n.mu.Lock()
		if n.token == token {
			n.token = ""
		}
		n.mu.Unlock()
		return err
	}
	if reply.ID == "" {
		return &qqRefillDeliveryError{message: "QQ 未返回消息 ID，发送结果不明；未自动重发"}
	}
	return nil
}

type AccountRefillNotificationSecrets struct {
	WeComWebhook   string
	QQClientSecret string
}

func (s *AccountRefillService) TestNotification(ctx context.Context, revision int64) error {
	return withAccountRefillConfigLock(ctx, s.store, 10*time.Second, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return errors.New("配置已变化，请刷新后测试")
		}
		if !refillNotificationConfigured(&state) {
			return errors.New("请先保存通知渠道和机器人凭据")
		}
		if time.Since(state.LastNotificationTest) < 30*time.Second {
			return errors.New("测试通知间隔至少 30 秒")
		}
		state.LastNotificationTest = time.Now()
		if err := saveRefillState(&state, save); err != nil {
			return err
		}
		s.sendRefillNotification(ctx, &state, "King 补号通知测试\n不会触发购买、导入或删除账号\n北京时间："+formatNotificationTime(state.LastNotificationTest))
		return saveRefillState(&state, save)
	})
}
