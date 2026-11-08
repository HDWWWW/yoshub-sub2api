# 可配置组合规则

后台入口：风控中心 → 审计配置 → 组合规则。可新增、修改、启停、删除规则，增删词组，导入或导出 JSON，并测试未保存的草稿。点击配置弹窗底部的保存后生效，其他实例通过现有设置缓存刷新，无须重启。

规则存储在 `content_moderation_config.combination_rules`。服务默认规则列表为空，不会因为升级而自动启用特定主题。`biological-research-rules.json` 是本站指定的可编辑候选配置，导入后可以修改或删除；没有词表写在匹配器中。

```json
{
  "combination_rules": [{
    "id": "example-topic",
    "name": "示例组合规则",
    "enabled": true,
    "action": "observe",
    "scope": "conversation",
    "window_characters": 400,
    "groups": [
      {"name": "对象", "keywords": ["猫", "cat"]},
      {"name": "动作", "keywords": ["画", "draw"]}
    ]
  }]
}
```

组内为 OR，组间为 AND，词序不限。匹配距离包含命中词本身，按 Unicode 字符计算，不能跨消息拼接。大小写不敏感，英文词边界避免 `map` 匹配到 `roadmap`。ID 必须唯一；每条 2–8 组，每组最多 200 个词、单词最多 200 字符，合计最多 100 条规则、5000 个词及 100000 个词表字符。导入追加规则，重复 ID 会报错。

`scope=conversation` 检查本次正文中的历史用户、助手、工具文本，不检查系统/开发者指令、图片或加密推理。工具续轮也检查已携带的历史消息。仅由 `previous_response_id` 等 ID 引用、未包含在正文中的历史不可读取。`current_user` 保持现有仅当前用户输入的边界。

`action=block` 在全局前置拦截模式下拒绝请求；`observe` 仅记录并继续其他审计。全局观察模式或风控白名单覆盖本地拦截。规则独立于普通关键词/API 策略，遵守风控开关、审计开关、分组和模型范围，在采样之前检查。任一启用规则要求拦截即拦截，多条命中合并到一条审计记录。

组合规则是站点主题筛选，不等同于上游 `bio_policy`。日志保留规则 ID、名称、匹配距离、来源消息、每组命中词及脱敏上下文，不伪造模型概率，不发送邮件，不写入违规哈希库，也不累计自动封禁次数。官方策略会话屏蔽仍由已有独立开关控制。

管理接口：`PUT /api/v1/admin/risk-control/config` 传 `combination_rules` 数组保存；省略该字段保留，空数组清空。`POST /api/v1/admin/risk-control/combination-rules/test` 传 `{ "rules": [...], "text": "..." }` 验证草稿，完全本地运行，不产生审计或外部 API 调用。测试接口文本最多 100000 字符，请求体最多 2 MB。
