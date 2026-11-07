# Yoshub 合并 Sub2API v0.2.20

官方版本：[v0.2.20](https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.20)，对应提交 `dab829a4e9b7d1cc307189aea8b0319eb29e29d0`。定制版本为 `0.2.20-yoshub.1`，发布标签为 `yoshub-v0.2.20-custom.1`。

## 官方主要变化

- 新增 OAuth 管理 MCP、授权确认和连接管理页面。仅在显式配置 `MCP_PUBLIC_URL` 后开放，默认关闭。
- 管理员 Key 增加细分权限和需文字确认的全权兼容模式，完善中英文权限说明。
- 新增注册邮箱精确黑名单，识别 Gmail 点号和加号别名；管理员可主动选择批量禁用命中的用户。
- OpenAI 图片账号可按 1K/2K/4K 分流，支持 GPT Image 2.6 原生图片和编辑参数。
- 修复长会话 cyber transcript 误拦截、Responses 非流式 incomplete/cancelled 和无 Content-Type 的 SSE 转 JSON、WS 桥接指纹和首 token 时间记录。
- 限制请求体、SSE、会话缓存和后台任务的内存占用，减少热路径 JSON 拷贝，完善 Anthropic/Antigravity/Gemini/Grok 兼容性。

## 保留和适配的定制功能

- 风控中心新增 Key 自动分配 IPv6、已有 Key 单独分配按钮、每个 Key 独占 IPv6。
- `bio_policy` / `cyber_policy` 保留原错误分类和停止自动重试行为；策略拒绝按原规则输出 200（已开始流）、400 或 403。
- 保留 SmartWS、优先级调度、账号风控、自动 Fast、补号、额度持久化和客户端取消传播。
- 新增全权兼容管理 Key 可使用定制管理接口；受限管理 Key 继续受原权限限制。
- SmartWS HTTP 桥接接入新版 SSE 限制配置；图片流通过 ResponseWriter.Write 写出，保留自定义断连检测和取消上游请求行为。
- API Key 授权缓存版本升级至 28，同时容纳 SmartWS 分组设置和上游隐私设置。

## 升级行为

- 请求体内存预算默认 1 GiB，配置键为 `gateway.request_body_memory_budget_bytes`，0 表示关闭。
- 每次上传读取和下游写出超时默认 120 秒，分别为 `gateway.request_body_read_timeout_seconds` 和 `gateway.downstream_write_timeout_seconds`。这是单次读写限制，不是整个流式请求的总时长。
- 上述限制对应的 408、413、503 等普通错误不属于 `bio_policy` 状态归一化范围。
- 新增 `249_mcp_oauth.sql`，只新增 3 张 OAuth 表及索引。迁移按完整文件名记录，与已有同编号的 Yoshub 迁移并存。
- 官方发布说明中的 `security.request_body_*` 前缀与源码不一致，此处以源码的 `gateway.*` 为准。
- 继续采用本地手动构建、私有镜像、不可变摘要和蓝绿切换。数据库、Redis、NewAPI 与 CPA 容器不随本次应用发布重建。
