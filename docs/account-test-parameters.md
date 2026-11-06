# 自定义账号测试请求

OpenAI 账号的“测试连接”普通文本模式支持填写测试消息和选择思考强度。测试消息为空时仍发送 `hi`，非空内容保留换行及首尾空格。思考强度为空时不发送该参数，使用上游默认；可选 `none / minimal / low / medium / high / xhigh / max`，由所选模型决定是否支持，不自动降档重试。

账号菜单中的“定时测试”在新建、编辑计划时保存相同参数，后台执行使用保存值。仅切换启用状态不会清空消息或思考强度；明确清空字段恢复默认。既有模型、Cron、启用状态及自动恢复开关保持原语义。图片测试不发送文本思考强度；Compact 能力探测仍使用专用请求。

手动接口 `POST /api/v1/admin/accounts/:id/test` 支持 `prompt`、`reasoning_effort`；定时计划创建、更新及读取接口使用相同字段。请求消息上限 32 KiB；未知强度或畸形 JSON 不派发请求。Responses 使用 `reasoning.effort`，Chat Completions 使用 `reasoning_effort`，OAuth/API Key 沿用现有认证与模型映射。

迁移 `255_scheduled_test_request_parameters.sql` 仅向 `scheduled_test_plans` 增加两个默认空字段，不修改旧计划；设置 5 秒锁等待上限。旧二进制仍可读写原有列，代码回退不删除新增字段。

参数依据：[OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning)。此改动不启用或修改分组风控检测、补号、吸能及其他恢复规则。
