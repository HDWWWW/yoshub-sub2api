export default {
  title: '风控检测', group: '检测分组', chooseGroup: '选择 OpenAI 分组', enabled: '启用此分组的风控检测',
  scope: '按分组观察可调度的 OpenAI 账号。容量 = 当前并发 / 账号并发上限；关闭本面板后仍在服务器运行。',
  multiTitle: '两个及以上账号：容量异常检测',
  multiHint: '按当前有效优先级（含到期排序、手动优先级和分组反转）取前两个账号。以下两条件连续满足才停首位账号；同优先级、采样缺失或条件中断会重置计时。会话绑定和模型限制也可能造成容量偏差，本规则不证明上游已风控。',
  firstThreshold: '首位容量低于 (%)', secondThreshold: '次位容量达到 (%)', duration: '持续时间（秒）',
  singleTitle: '一个账号：定时消息检测',
  singleHint: '默认每 300 秒发送一次。只检查完整回复的可见文本是否包含期望内容（区分大小写）；网络错误、限流和不完整回复不作为内容不匹配。测试会消耗该账号的上游额度，不计入用户账单。',
  interval: '测试间隔（秒）', model: '模型（可自填）', effort: '思考强度', upstreamDefault: '上游默认', prompt: '测试消息', expected: '回复必须包含',
  stopHint: '触发后仅将异常账号的调度关闭，现有请求继续完成；不会自动恢复。账号调度开关由所有关联分组共享，恢复请在账号列表手动开启。',
  status: '当前检测状态', active: '已启用', disabled: '已关闭', stale: '近期未收到采样，请检查服务状态。', first: '首位账号', second: '次位账号',
  since: '连续异常开始：', nextProbe: '下一次测试：', history: '最近自动停调度记录', saved: '配置已保存', loadError: '读取风控检测失败', saveError: '保存风控检测失败'
}
