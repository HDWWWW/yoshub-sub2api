# OVH IPv6 分配入口

账号创建、编辑、批量创建、批量编辑、Codex session 导入、JSON 数据导入、
OpenAI PAT / OAuth 创建、Grok SSO / OAuth 创建以及自动补号都支持
`proxy_mode: "ovh_ipv6"`。每个成功保存的母账号领取一个全新 IPv6；编辑已有
账号会更换为全新地址，旧地址永久退役。影子账号继续继承母账号，不能独立分配。

此显式模式不依赖 IP 池中的“随机代理使用 OVH IPv6”开关。该开关只改变
`random` 的来源；`direct`、`specified`、`least_used` 保持原有含义。
自动分配请求不能同时传正数 `proxy_id`。出口服务失败时返回错误，不回退直连
或手动代理池。代理分配和账号保存共用事务；失败领取消耗的编号永不回退。
批量更新最多同时处理 8 个账号，每个账号独立提交，重复 ID 只处理一次。
结果按输入顺序返回；成功项包含 `proxy_id` 和实际 `managed_ipv6`，失败项保留
原绑定和原设置并报告失败原因。请求取消后不再开始新分配。

## 手动领取未绑定地址

完整管理员可调用：

```http
POST /api/v1/admin/proxies/ovh-ipv6/allocate
Content-Type: application/json
Idempotency-Key: <unique request key>

{"name":"待绑定 IPv6"}
```

`name` 可省略，最长 100 字符。响应为普通 Proxy DTO，包括 `managed_ipv6`
和 `managed_ipv6_state: "reserved"`，不返回代理密码。每次操作只领取一个地址，
不预生成地址段；记录立即显示在代理池，随后通过 `specified` 和 `proxy_id`
绑定给账号。已有受限管理密钥不能领取池地址；其账号操作仍须具备
`accounts.network` 权限才可显式分配或指定预留地址。

生命周期只有以下方向：

```text
手动领取 → reserved → 第一次绑定 → bound → 删除账号或换 IP → retiring → 清理
              └──── 删除预留代理 ────────────────────────┘
账号自动分配 ──────────────────→ bound
```

首次绑定由数据库行锁保证独占；绑定后不能再次作为空闲代理使用。
删除预留代理使用现有单条或批量删除接口。仅旧连接排空期间保留清理记录，
最终删除代理和租约行，宿主机高水位持续保留，不保存逐地址退役历史。
预留租约提交后立即激活，因此不会被出口进程当作未提交临时租约超时回收。

## 导入、导出与复制

JSON 导入请求和各 `DataAccount` 支持 `proxy_mode`、`proxy_id`；请求级选择
优先于各条目。模式留空时沿用文件代理；显式选择会覆盖文件代理，包括旧备份
中的“需要全新分配”标记。`ovh_ipv6` 总是创建新地址，不能恢复旧认证信息。
托管 IPv6 认证信息仍不导出，手工复制其用户名、密码或代理键继续被拒绝。
复制使用托管 IPv6 的账号时，新副本领取新地址，并保留副本默认暂停调度的行为。

OAuth 登录、兑换或验证阶段仍使用请求中的实际正数代理 ID；自动模式在保存
账号时分配。若登录阶段也需要指定 IPv6，应先手动领取预留地址，再指定该代理。

自动补号的指定代理模式不接受任何托管 IPv6（包括预留地址），避免多个付费任务
竞争同一个独占地址；应选择 `ovh_ipv6` 为每次交付分配新地址。普通随机池及
最空闲池也排除托管 IPv6，手动预留地址只用于明确指定的账号保存。

## 部署与验证

本次新增 `252_ovh_ipv6_reserved.sql`，不修改已部署的迁移 251。
出口进程协议及状态文件不变。旧、新应用实例可以并存；数据库触发器约束
两者的绑定和删除，后台清理请求幂等。

隔离 PostgreSQL 测试覆盖显式模式、预留首次绑定、并发独占、账号换 IP、
空闲地址删除、事务回滚、服务故障和双实例清理；服务测试覆盖创建、编辑和
批量事务边界，handler 测试覆盖管理员权限与导入覆盖规则。

## 存储占用统计

完整管理员通过 `GET /api/v1/admin/proxies/ovh-ipv6/storage` 按需读取统计。
接口不分配地址、不清理文件，也不读取账号或代理凭据。每次响应带
`sampled_at`，三个数据源分别采样，不保证是同一个事务快照：

- `issued` 为现有出口控制接口返回的十进制字符串 N，可超过 JavaScript 安全整数。
  编号 `1–N` 都已消耗且永不重新发放，其中既有当前仍在用的地址，也有已退役
  或分配失败的地址；不能把 N 当作已退役数量。控制服务不可读时为 `null`。
- `files.items` 仅包含 `counter.json`、`counter.guard.json`、`leases.json`。
  `bytes` 是 `stat` 的真实文件长度，Linux `disk_bytes` 是 `st_blocks × 512`
  的实际分配空间；无法取得时为 `null`，不会用估算值或零代替。
  `counter_bytes` / `counter_disk_bytes` 合计双计数文件，`total_bytes` /
  `total_disk_bytes` 合计三个固定文件。任一对应文件不可统计时，该合计为 `null`。
- `database` 的 `rows` / `reserved` / `bound` / `retiring` 是当前租约行数。
  `table_bytes`、`index_bytes`、`total_bytes` 分别来自 PostgreSQL
  `pg_table_size`、`pg_indexes_size`、`pg_total_relation_size`，只包含专属
  `yoshub_ovh_ipv6_leases` 表及其索引，不代表整个数据库、共享 `proxies` 表、
  WAL 或备份占用。删除后物理页可能留待复用，因此零行不等于零字节。

双计数文件各保存同一个 N，而不是逐地址退役清单。它们的长度只随 N 的十进制
位数增长；`leases.json` 和数据库租约表仅保留预留、当前绑定及未清理完成的租约。
完成退役后删除租约记录，不保留“这个地址过去属于哪个账号”的历史。

应用配置 `OVH_IPV6_STATE_STATS_DIR=/run/ovh-ipv6-state`，并将宿主状态**目录**
只读挂载到该路径，例如 `/var/lib/yoshub-ipv6-egress:/run/ovh-ipv6-state:ro`。
不能只挂载三个单文件：出口服务用 atomic rename 替换文件，单文件挂载可能一直
指向旧 inode，导致统计过期。应用只对固定文件调用 `Lstat`，拒绝符号链接和
非普通文件；不打开内容、不枚举目录，也不显示宿主绝对路径。

目录保持 root 所有，三个文件保持 `0600`。仅给应用实际 UID 添加目录 traverse
权限，例如 `setfacl -m u:1000:--x /var/lib/yoshub-ipv6-egress`，不授予目录读取
或文件读取权限。核验时使用实际应用 UID，不能以 `docker exec` 默认 root 的
结果代替；应用应能 `stat` 三个文件，但不能 `cat leases.json` 或列出目录。
systemd 的 `StateDirectoryMode=0700` 可能在后续启动时重设 ACL mask，部署恢复
流程需重新核验 traverse 权限。此功能只需将挂载和环境变量加入新的应用实例，
不修改、重启 IPv6 出口进程；缺少挂载或权限时明确返回不可用统计。

配套 `deploy/ovh-ipv6-egress/storage-stats-acl.py` 仅修改状态目录 ACL，
不打开文件；`storage-stats.conf` 在今后的服务启动前重新应用 traverse 权限。
部署时安装辅助脚本和 systemd drop-in，执行一次辅助脚本并 daemon-reload 即可，
不要为统计功能重启正在承载 CONNECT 连接的出口进程。

## 批量结果与预估补号开关

编辑账号时，账号详情返回当前代理的 `managed_ipv6` 和 `managed_ipv6_state`。
编辑窗口把当前绑定合并到代理选项，并优先使用详情中的新数据，因此导入或自动补号
在页面打开后创建的 IPv6 也能直接回显。未改变代理选择时保存不会重新分配地址。
已绑定代理详情缺失时显示代理 ID 与“详情未加载”，不会误显示为“无代理”。

自动代理批量更新最多同时处理 8 个账号，每个账号独立事务，去重并按原顺序返回。
成功项附带 `proxy_id` 与 `managed_ipv6`，失败项返回错误且保留旧绑定。
前端保留结果面板，避免成功后误操作再次分配；自动模式请求单独等待最多 5 分钟。
超时只表示结果未确认，应先刷新核对实际绑定，不能假定全部失败。

补号规则的 `forecast_enabled` 为可选布尔值，缺省保持预估并发提前补号开启。
显式关闭后不计算退出后的预测容量、不触发预估补号，并清除预估持续观察状态；
当前并发、最低库存、固定和定时规则保持原行为。在途订单继续使用提交时的交付快照。
重新开启要重新达到持续观察时间。旧客户端未传字段时保留已保存选择。
发布该字段的首次蓝绿更新期间，须暂时阻止修改补号规则，待旧实例自然排空并停止后
恢复保存，防止旧版本后台写回状态时丢弃新字段。
