# 分组加速模式与账号初始化

本次版本：`0.2.19-yoshub.speed-modes.1`。

## 分组设置

OpenAI 与 Composite 分组支持三个互斥开关；Composite 仅影响实际选中的 OpenAI 账号。

| 开关 | 请求处理 |
| --- | --- |
| 强制 Fast | 写入 `service_tier=priority` |
| 强制 Ultrafast | 写入 `service_tier=ultrafast` |
| 过滤 service_tier | 移除请求里的档位字段，包括 Fast、Ultrafast、Flex；上游使用自己的默认档位 |

界面切换任一开关会关闭另外两项。管理 API 新增 `force_openai_ultrafast`、`filter_openai_service_tier`；两者默认关闭。创建和更新会拒绝冲突组合，数据库 CHECK 约束进一步防止双开。PATCH 风格更新省略字段时保留旧值，因此接口切换模式应同时将原模式设为 false。

全局档位策略仍然生效：强制模式先写入档位，然后应用全局策略；分组过滤保留全局 block，但覆盖 pass/force_priority，防止被重新添加档位。HTTP、OAuth 自动透传、原生 Responses 与 WebSocket response.create 使用相同规则。WebSocket 其他帧保持原样。保存后新请求读取新配置；已有 WebSocket 会话需重连。

“免费 Fast / Ultrafast”统一使用现有 `free_openai_fast` 字段，同时覆盖两个加速档位。请求仍使用选中的上游档位，客户按同请求的 Standard 价格及分组倍率扣费；使用记录保留实际请求档位与加速档位成本。该开关不免除上游成本，不影响 Flex 折扣。Ultrafast 需要上游账号和模型支持，不承诺固定速度倍数。

## 快乐首字

管理侧栏增加“快乐首字”，可配置固定或随机延时，默认范围 100–1000 ms，默认关闭。此功能仅在 OpenAI OAuth 的 Responses SSE 已取得上游成功响应后发送一次 `response.metadata`，不发送账号额度或伪造 `codex.rate_limits`。提前发送的通知不计入 Sub2 正文 TTFT；正文先到则立即输出。

HTTP 认证/限流错误继续使用原状态码。通知后的流式失败继续走原有错误识别和账号切换；最终错误通过 SSE 失败事件回传。带 Codex turn-state 的响应及 compact 路径沿用原逻辑，不提前提交会话状态。通知不是模型生成加速，也不能保证在上游连接建立前达到设定时间。

## 未分组账号初始化

`token_refresh.ungrouped_check_interval_seconds` 默认 5 秒，设为 0 可关闭。后台扫描符合条件的未分组 OpenAI OAuth 账号，复用令牌锁、刷新重试与限速，然后读取并持久化额度，更新调度缓存。只扫描活动、含 refresh token、未临时禁用的适用账号；排除 PAT、代理身份与影子账号。

扫描使用游标分页、共享并发和速率门、禁止周期重叠。处理前后重新读取账号，防止自动推送并发分组覆盖；保留现有预设、分组归属及账号错误处理。额度查询失败不将有效令牌误判为失效。5 秒是扫描间隔，刷新和上游额度接口耗时不包含在此承诺内。

## 验证与发布

覆盖开关互斥、创建/编辑序列化、分组复制、鉴权缓存、HTTP/WS 档位覆盖、全局策略组合、免费扣费及长上下文重算。数据库集成测试验证新增字段持久化与互斥约束。快乐首字验证延时、一次发送、正文直出、限流、失败重试与停止；账号扫描验证锁内分组变化、分页恢复、额度写入失败及退出取消。发布使用已提交源码构建的私有 linux/amd64 镜像，只更新 Sub2 app。
