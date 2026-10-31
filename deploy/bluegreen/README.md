# Sub2API 蓝绿发布

当前生产为 `dedicated-YosHub`，正式清单 `/opt/yoshub-production/sub2api.compose.json`，
操作脚本 `/opt/yoshub-production/sub2api-operate.py`。仅发布本机已验证的私有不可变镜像。

固定路由容器 `sub2api-router` 同时提供 Docker DNS `yoshub-sub2api` / `sub2api` 和
127.0.0.1:8084。外部域名由原宿主 Nginx 转到 8084，NewAPI 内网调用继续使用原域名和端口。
应用槽位 blue/green 的本机端口为 8082/8083，不对公网开放。数据库、Redis 和其他应用不重建。

```bash
sudo -n python3 /opt/yoshub-production/sub2api-operate.py status
sudo -n python3 /opt/yoshub-production/sub2api-operate.py deploy 'ghcr.io/hdwwww/yoshub-sub2api:<sha>@sha256:<digest>'
# 排空超时保留旧实例，之后继续：
sudo -n python3 /opt/yoshub-production/sub2api-operate.py finish '<同一镜像>' --timeout 900
sudo -n python3 /opt/yoshub-production/sub2api-operate.py rollback blue
```

先启动空闲槽位，健康后热加载固定路由，再检查旧应用网络命名空间中所有 8080 入站连接，
覆盖外部 HTTP、WebSocket 和 Docker 内网连接。连续三次为空才停旧实例；超时不强杀。
Nginx 配置验证/路由验证失败恢复旧配置。应用切流不重启固定路由容器。
Sub2API 没有 NewAPI 的独立 master/slave 角色，排空后旧槽位停机，避免长期重复后台任务；
下一次发布交换槽位。补号使用既有跨实例 PostgreSQL 锁和幂等订单。

首次安装 `bootstrap` 保留原 `yoshub-sub2api` 服务直到其连接排空，保存配置备份，
完成后从旧汇总清单移除该服务，旧单服务清单变为新清单的兼容入口。
首次迁移在旧容器网络命名空间内使用只匹配旧 IP:8080 TCP SYN 的临时 NAT 规则，
将新建连接转到路由容器；不依赖宿主 bridge netfilter，也不改变宿主网络 sysctl。
Linux conntrack 保持已经建立的连接不变，排空停机后该命名空间及其规则一起释放。
汇总清单包含新蓝绿清单，现有生产恢复工具能发现活动槽位及路由容器，备用槽位由 profile 排除。
如果原内网客户端持续复用旧连接，保留旧实例直到排空后再执行 `finish`。
不得用 `docker compose down` 或强制重建正在承载请求的槽位来结束排空。

蓝绿仅保护应用发布期间的连接。服务器故障、网络中断或上游自身断流仍可能导致请求失败。
数据库不兼容迁移不应通过此脚本自动发布。IP 容量扩展只新增独立设置表，旧版本忽略该表，
支持切流期间两版本并存；已有账号和代理记录不重写。
