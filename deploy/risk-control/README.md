# codex2api 本地组合拦截

入口：风控中心 → 审计配置 → 组合拦截 · codex2api。

本地规则核心与 130 条预设来自 `james-6-23/codex2api` 的 `security/promptfilter`，固定版本 `b6a827f9357a6cf1ac4c211854726d808406c44f`。上游源码、测试和逐文件 SHA-256 位于 `backend/internal/pkg/codexpromptfilter`。保持原版正则、AND/OR/排除条件、信号分数、严格规则、语境折扣、编码归一化、来源分类、长文本预算和工具输出高置信例外；不将所有消息拼成一段做正则匹配。

配置保存到 `content_moderation_config.rule_filter`。新安装默认关闭；本站迁移时明确启用 block，普通阈值 50、严格阈值 90、strict_terminal_enabled=true，启用全部预设并删除旧 biological-research-combination。升级程序本身不覆盖管理员已有配置。旧 combination_rules 和旧日志继续支持读取，但启用新引擎后不执行旧匹配器。

后台支持查看和搜索全部预设、分类筛选、逐条启停、自定义正则的增删改、JSON 导入导出、恢复推荐值、草稿测试。内置规则是版本化源码，禁用列表和自定义规则均可随时修改并热更新。导入替换当前规则草稿；恢复推荐值保留自定义规则，恢复阈值及内置启停。两者都需点击页面保存。自定义规则最多 100 条，配置最多 1 MiB；使用上游 AuditPatternConfig 验证，错误正则、空匹配、过宽规则及重复名称会返回明确错误。

主正则与 all_patterns 同时满足；any_patterns 至少命中 min_matches 条（0 表示默认 1）；exclude_patterns 任一命中排除。AuthorizationExcludePatterns 保留原版字段，推荐策略不承认无法验证的授权自述。大小写、匹配顺序等遵从 Go RE2 和原版归一化逻辑，需要忽略大小写时使用 (?i)。Strict 优先于 SignalOnly，二者同时启用仍可能拦截。模式 monitor/warn/block 通过原版 pipeline 限制实际动作。

本层仍遵守 Sub2 风控/审计开关、分组、模型范围、全局观察模式和白名单，在采样之前执行。保留独立的 blocked_keywords、keyword_blocking_mode、现有 OpenAI/TypeSafe API 审核和 Qwen 提示词审计配置。规则拦截使用既有 block_status/block_message。只迁移本地规则层，不启用 codex2api 的外部复核服务、跨请求会话拼接或账户处罚；不能宣称两套完整服务的所有行为相同。

日志主正文继续使用最近有效用户正文；规则依据单独记录版本、动作、分数、阈值、来源、命中规则与脱敏片段，前端用文本插值高亮。分数不是审核 API 的概率。辅助内容通过既有有界工作队列观察，最多 2 个并行检测，队列拥挤时丢弃并计入 dropped，不影响同步拦截。规则日志不写违规 hash、不发邮件、不累计封禁次数。历史组合规则日志保留原格式显示。

管理接口：

- `GET /api/v1/admin/risk-control/rule-filter/catalog`：版本、预设及推荐配置。
- `PUT /api/v1/admin/risk-control/config`：传 `rule_filter` 保存完整对象；省略则保留。
- `POST /api/v1/admin/risk-control/rule-filter/test`：传 `{config, text}`；或 `{config, body, protocol}` 检查协议与来源。只做本地计算，不保存、不写审计、不请求模型。测试使用草稿的 enabled 开关。

旧官方 bio_policy/cyber_policy 日志仍按用户正文投影展示，既有官方错误会话屏蔽由原功能控制。
