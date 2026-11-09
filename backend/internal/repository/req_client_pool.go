package repository

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"

	"github.com/imroc/req/v3"
)

// reqClientOptions 定义 req 客户端的构建参数
type reqClientOptions struct {
	ProxyURL    string        // 代理 URL（支持 http/https/socks5）
	Timeout     time.Duration // 请求超时时间
	Impersonate bool          // 是否模拟浏览器指纹（当前为 Firefox，Chrome 伪装会被 chatgpt.com 的 Cloudflare 质询）
	ForceHTTP2  bool          // 是否强制使用 HTTP/2
}

// sharedReqClients 存储按配置参数缓存的 req 客户端实例
//
// 性能优化说明：
// 原实现在每次 OAuth 刷新时都创建新的 req.Client：
// 1. claude_oauth_service.go: 每次刷新创建新客户端
// 2. openai_oauth_service.go: 每次刷新创建新客户端
// 3. gemini_oauth_client.go: 每次刷新创建新客户端
//
// 新实现使用 sync.Map 缓存客户端：
// 1. 相同配置（代理+超时+模拟设置）复用同一客户端
// 2. 复用底层连接池，减少 TLS 握手开销
// 3. LoadOrStore 保证并发安全，避免重复创建
var sharedReqClients sync.Map
var sharedReqClientPruneMu sync.Mutex
var sharedReqClientLastPrune atomic.Int64

const sharedReqClientCapacity = 4096
const sharedReqClientIdleTTL = 10 * time.Minute

type sharedReqClientEntry struct {
	client   *req.Client
	lastUsed atomic.Int64
}

func pruneSharedReqClients(now time.Time, force bool) {
	if !force && now.Unix()-sharedReqClientLastPrune.Load() < 60 {
		return
	}
	sharedReqClientPruneMu.Lock()
	defer sharedReqClientPruneMu.Unlock()
	if !force && now.Unix()-sharedReqClientLastPrune.Load() < 60 {
		return
	}
	sharedReqClientLastPrune.Store(now.Unix())
	type candidate struct {
		key   any
		entry *sharedReqClientEntry
		used  int64
	}
	var entries []candidate
	sharedReqClients.Range(func(k, v any) bool {
		e, ok := v.(*sharedReqClientEntry)
		if !ok {
			return true
		}
		used := e.lastUsed.Load()
		if now.Sub(time.Unix(0, used)) > sharedReqClientIdleTTL {
			if sharedReqClients.CompareAndDelete(k, e) {
				e.client.GetTransport().CloseIdleConnections()
			}
		} else {
			entries = append(entries, candidate{k, e, used})
		}
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].used < entries[j].used })
	for i := 0; i < len(entries)-sharedReqClientCapacity; i++ {
		e := entries[i]
		if sharedReqClients.CompareAndDelete(e.key, e.entry) {
			e.entry.client.GetTransport().CloseIdleConnections()
		}
	}
}

// getSharedReqClient 获取共享的 req 客户端实例
// 性能优化：相同配置复用同一客户端，避免重复创建
func getSharedReqClient(opts reqClientOptions) (*req.Client, error) {
	pruneSharedReqClients(time.Now(), false)
	key := buildReqClientKey(opts)
	if cached, ok := sharedReqClients.Load(key); ok {
		if e, ok := cached.(*sharedReqClientEntry); ok {
			e.lastUsed.Store(time.Now().UnixNano())
			return e.client, nil
		}
	}

	client := req.C().SetTimeout(opts.Timeout)
	if opts.ForceHTTP2 {
		client = client.EnableForceHTTP2()
	}
	if opts.Impersonate {
		// chatgpt.com 的 Cloudflare 会对 req 内置的 Chrome 伪装（UA 固定为 Chrome/120，
		// 与 sec-ch-ua 等 Client Hints 一起已明显过时）直接返回 403 cf-mitigated=challenge，
		// 导致 accounts/check、subscriptions、隐私设置等 backend-api 调用全部失败，
		// 订阅到期时间因此长期不更新（见 issue #4825）。Firefox 伪装的 UA/头部组合
		// 在同一出口 IP 下稳定通过，故改用 Firefox 指纹。
		client = client.ImpersonateFirefox()
	}
	trimmed, _, err := proxyurl.Parse(opts.ProxyURL)
	if err != nil {
		return nil, err
	}
	if trimmed != "" {
		client.SetProxyURL(trimmed)
	}
	client = instrumentReqClient(client)

	entry := &sharedReqClientEntry{client: client}
	entry.lastUsed.Store(time.Now().UnixNano())
	actual, loaded := sharedReqClients.LoadOrStore(key, entry)
	if e, ok := actual.(*sharedReqClientEntry); ok {
		if loaded {
			client.GetTransport().CloseIdleConnections()
		}
		e.lastUsed.Store(time.Now().UnixNano())
		return e.client, nil
	}
	return client, nil
}

func instrumentReqClient(client *req.Client) *req.Client {
	if client == nil {
		return nil
	}
	client.GetTransport().WrapRoundTripFunc(func(rt http.RoundTripper) req.HttpRoundTripFunc {
		timed := servertiming.WrapRoundTripper(rt)
		return timed.RoundTrip
	})
	return client
}

func buildReqClientKey(opts reqClientOptions) string {
	return fmt.Sprintf("%s|%s|%t|%t",
		strings.TrimSpace(opts.ProxyURL),
		opts.Timeout.String(),
		opts.Impersonate,
		opts.ForceHTTP2,
	)
}

// CreatePrivacyReqClient creates an HTTP client for OpenAI privacy settings API
// This is exported for use by OpenAIPrivacyService
// Uses Chrome TLS fingerprint impersonation to bypass Cloudflare checks
func CreatePrivacyReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL:    proxyURL,
		Timeout:     30 * time.Second,
		Impersonate: true, // Enable browser TLS fingerprint impersonation (Firefox, see getSharedReqClient)
	})
}
