# OpenAI OAuth 账号隐私开关

账号创建、编辑和补号推送设置提供双向隐私开关：开启时请求关闭 OpenAI 训练数据共享，关闭时请求允许共享。没有保存过选择的账号和补号规则继续默认开启，不改变原有导入、购买、交付和重试流程。

`privacy_enabled` 是可选布尔字段，服务保存为 `extra.openai_privacy_enabled`。省略字段的普通编辑不会改变已有选择；Session、Agent Identity、PAT 和批量创建透传显式的 `false`。令牌刷新和后台隐私设置使用最新的持久化选择，不会把关闭状态恢复为默认开启。影子账号继承母账号，需在母账号上修改。

`privacy_mode` 单独记录执行结果：`training_off` 显示 Private，`training_on` 显示 Shared；执行失败仍显示 Fail。保存选择不代表上游已经确认成功，界面根据实际结果提示。要求隐私保护的分组继续排除未确认关闭共享的账号。

`POST /api/v1/admin/accounts/:id/set-privacy` 接受可选 JSON `{ "enabled": false }`；空请求保持原先开启隐私的行为。账号编辑接受 `privacy_enabled`，补号规则使用 `delivery.privacy_enabled`，已受理的订单保留提交时的选择。

并发保护包括：网络请求前后读取当前选择、仅在选择仍匹配时写入结果、事务内发布调度更新，以及行锁下保留普通编辑没有显式修改的隐私字段。若旧请求完成时发现选择已改变，会在有界时限内按最新选择纠正上游状态。
