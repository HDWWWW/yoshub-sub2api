export default {
  title: '自动Fast', group: '规则分组', chooseGroup: '选择 OpenAI 分组', enabled: '启用此分组的自动Fast',
  scope: '按账号的号商回收时间判断；没有回收时间标记时使用实际到期时间。每 10 秒检查一次，关闭面板后仍在服务器运行。',
  stagesTitle: '分阶段开关 Fast',
  stagesHint: '时间从大到小排列。进入下一阶段后，以新阶段为准：7D已用比例低于阈值就开Fast，达到阈值就关Fast。例如45分钟阶段开启后，进入30分钟阶段不满足新条件就关闭。',
  minutes: '剩余时间不超过（分钟）', usedBelow: '7D已用低于（%）', addStage: '添加阶段', removeStage: '删除第 {index} 个阶段',
  finalMinutes: '最后多少分钟持续强制Fast', finalHint: '必须小于最后一个普通阶段；此阶段无论7D用了多少都会开启Fast，直到回收或到期。',
  maxAge: '7D数据有效期（秒）', maxAgeHint: '普通阶段遇到数据缺失或过期时保持当前状态，等待新数据。不会自动发送测试消息。',
  restoreHint: '同一账号只允许一套规则接管，账号Fast开关对其所有分组生效。启用期间手动开关可能被下一轮规则覆盖；关闭自动Fast、离开分组或退出时间段时恢复接管前状态。',
  invalid: '请检查：阶段时间必须递减，最终时间更小，比例为0–100%，数据有效期为30–86400秒。',
  conflict: '此分组配置已在其他页面修改。请重新载入后再编辑，避免覆盖他人的设置。', reload: '重新载入配置（丢弃未保存修改）',
  saved: '已保存，下一轮检查生效', status: '当前运行状态', active: '已启用', disabled: '已关闭', total: '观察 {count} 个账号',
  checked: '最近检查：', stale: '等待检查，或近期未收到状态，请稍后刷新。', managed: '自动接管中', unmanaged: '未接管',
  supplier: '号商回收时间', imported: '导入回收时间', local: '实际到期时间', unknown: '数据未知或过期', currentStage: '当前阶段：剩余不超过 {minutes} 分钟',
  truncated: '仅展示前200个账号；规则对分组内所有符合条件的账号生效。', history: '最近Fast变更', loadError: '读取自动Fast失败', saveError: '保存自动Fast失败'
}
