import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper, VueWrapper } from '@vue/test-utils'

import RiskControlView from '../RiskControlView.vue'
import { defaultRuleFilter } from '@/api/admin/riskControl'
import type { ContentModerationAPIKeyStatus, ContentModerationConfig, ContentModerationLog, UpdateContentModerationConfig } from '@/api/admin/riskControl'

const {
  getConfig,
  updateConfig,
  getStatus,
  listLogs,
  getLogInput,
  getGroups,
  getProxies,
  testAPIKeys,
  allocateAPIKeyIPv6,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getConfig: vi.fn(),
  updateConfig: vi.fn(),
  getStatus: vi.fn(),
  listLogs: vi.fn(),
  getLogInput: vi.fn(),
  getGroups: vi.fn(),
  getProxies: vi.fn(),
  testAPIKeys: vi.fn(),
  allocateAPIKeyIPv6: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    riskControl: {
      getConfig,
      updateConfig,
      getStatus,
      listLogs,
      getLogInput,
      testAPIKeys,
      allocateAPIKeyIPv6,
      deleteFlaggedHash: vi.fn(),
      clearFlaggedHashes: vi.fn(),
      unbanUser: vi.fn(),
    },
    groups: {
      getAll: getGroups,
    },
    proxies: {
      getAll: getProxies,
    },
  },
}))

vi.mock('@/api/admin/riskControl', async (importOriginal) => ({ ...(await importOriginal<typeof import('@/api/admin/riskControl')>()), getRuleFilterCatalog: vi.fn().mockResolvedValue({ version: 'test', presets: [], defaults: { enabled: false, mode: 'block', threshold: 50, strict_threshold: 90, strict_terminal_enabled: true, custom_patterns: [], disabled_patterns: [] } }) }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
  }),
}))

vi.mock('@/utils/apiError', () => ({
  extractApiErrorMessage: (_err: unknown, fallback: string) => fallback,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        if (key === 'admin.riskControl.preBlockAPIKeyLoadSummary') {
          return `同步并发 ${params?.active} / 可用 Key ${params?.available}，累计 ${params?.total} 次，worker：${params?.workerActive} / ${params?.workerTotal}`
        }
        return key.replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? `{${token}}`))
      },
    }),
  }
})

const baseConfig = (): ContentModerationConfig => ({
  enabled: true,
  mode: 'pre_block',
  base_url: 'https://api.openai.com',
  model: 'omni-moderation-latest',
  proxy_id: null,
  api_key_configured: false,
  api_key_masked: '',
  api_key_count: 0,
  api_key_masks: [],
  api_key_statuses: [],
  timeout_ms: 3000,
  sample_rate: 100,
  all_groups: true,
  group_ids: [],
  record_non_hits: false,
  worker_count: 4,
  queue_size: 32768,
  block_status: 403,
  block_message: '内容审计命中风险规则，请调整输入后重试',
  email_on_hit: true,
  auto_ban_enabled: true,
  ban_threshold: 10,
  violation_window_hours: 720,
  retry_count: 2,
  hit_retention_days: 180,
  non_hit_retention_days: 3,
  pre_hash_check_enabled: false,
  blocked_keywords: [],
  keyword_blocking_mode: 'keyword_and_api',
  thresholds: {
    harassment: 0.98,
    sexual: 0.65,
  },
  model_filter: {
    type: 'all',
    models: [],
  },
})

const runtimeStatus = () => ({
  enabled: true,
  risk_control_enabled: true,
  mode: 'pre_block',
  worker_count: 4,
  max_workers: 32,
  active_workers: 0,
  idle_workers: 4,
  queue_size: 32768,
  queue_length: 0,
  queue_usage_percent: 0,
  enqueued: 0,
  dropped: 0,
  processed: 0,
  errors: 0,
  pre_block_active: 0,
  pre_block_checked: 0,
  pre_block_allowed: 0,
  pre_block_blocked: 0,
  pre_block_errors: 0,
  pre_block_avg_latency_ms: 0,
  pre_block_api_key_active: 0,
  pre_block_api_key_available_count: 0,
  pre_block_api_key_total_calls: 0,
  pre_block_api_key_loads: [],
  api_key_statuses: [],
  flagged_hash_count: 0,
  last_cleanup_deleted_hit: 0,
  last_cleanup_deleted_non_hit: 0,
})

const AppLayoutStub = { template: '<div><slot /></div>' }
const BaseDialogStub = defineComponent({
  props: {
    show: {
      type: Boolean,
      default: false,
    },
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})
const ModelWhitelistSelectorStub = defineComponent({
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  setup(props, { emit }) {
    const onInput = (event: Event) => {
      const value = (event.target as HTMLInputElement).value
      emit(
        'update:modelValue',
        value
          .split(/[,\n]/)
          .map((item) => item.trim())
          .filter(Boolean)
      )
    }
    return () =>
      h('input', {
        'data-test': 'model-filter-input',
        value: (props.modelValue as string[]).join('\n'),
        onInput,
      })
  },
})

function findButtonByText(wrapper: VueWrapper, text: string): DOMWrapper<HTMLButtonElement> {
  const button = wrapper.findAll<HTMLButtonElement>('button').find((item) => item.text().includes(text))
  if (!button) {
    throw new Error(`button not found: ${text}`)
  }
  return button
}

describe('admin RiskControlView', () => {
  beforeEach(() => {
    getConfig.mockReset()
    updateConfig.mockReset()
    getStatus.mockReset()
    listLogs.mockReset()
    getLogInput.mockReset()
    getGroups.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
    testAPIKeys.mockReset()
    allocateAPIKeyIPv6.mockReset()

    getConfig.mockResolvedValue(baseConfig())
    getStatus.mockResolvedValue(runtimeStatus())
    listLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })
    getLogInput.mockResolvedValue({ input_excerpt: '历史摘要', matched_keyword: '', evidence: null })
    getGroups.mockResolvedValue([])
    getProxies.mockResolvedValue([])
    updateConfig.mockImplementation(async (payload: UpdateContentModerationConfig) => ({
      ...baseConfig(),
      ...payload,
      model_filter: payload.model_filter ?? baseConfig().model_filter,
      api_key_configured: false,
      api_key_masked: '',
      api_key_count: 0,
      api_key_masks: [],
      api_key_statuses: [],
    }))
  })

  const auditRow = (id = 1) => ({
    account_id: null as number | null, account_name: '',
    id, created_at: '2026-11-08T04:00:00Z', user_email: 'writer@example.test',
    endpoint: '/v1/responses', provider: 'openai', model: 'test-model', mode: 'pre_block',
    action: 'keyword_block', flagged: true, highest_category: 'keyword', highest_score: 1,
    matched_keyword: '仿生', input_excerpt: '…角色穿上仿生装甲走入画面。…',
    error: '', violation_count: 1, category_scores: {}, threshold_snapshot: {},
  })
  const mountAudits = async (rows = [auditRow()]) => {
    listLogs.mockResolvedValue({ items: rows, total: rows.length, page: 1, page_size: 20, pages: 1 })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    return wrapper
  }
  const auditEvidence = (text: string) => ({ input_excerpt: '摘要', matched_keyword: '仿生', evidence: {
    text, total_characters: [...text].length, stored_characters: [...text].length,
    truncated: false, match_context: '角色穿上仿生装甲走入画面。',
  } })

  it('loads evidence on demand and highlights a match beyond the retained input', async () => {
    getLogInput.mockResolvedValue({ ...auditEvidence('长剧本正文。'.repeat(2400)), evidence: {
      text: '长剧本正文。'.repeat(2400), total_characters: 20000, stored_characters: 12000,
      truncated: true, match_context: '角色穿上仿生装甲走入画面。',
    } })
    const wrapper = await mountAudits()
    expect(getLogInput).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="moderation-input-1"] mark').text()).toBe('仿生')
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(getLogInput).toHaveBeenCalledWith(1)
    expect(wrapper.get('[data-test="moderation-input-retention"]').text()).toContain('inputTruncated')
    expect(wrapper.get('[data-test="moderation-match-context"] mark').text()).toBe('仿生')
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe('长剧本正文。'.repeat(2400))
    wrapper.unmount()
  })

  it('explains missing legacy evidence and supports retry after a failed fetch', async () => {
    getLogInput.mockRejectedValueOnce(new Error('network failed'))
    const wrapper = await mountAudits()
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('inputDetailFailed')
    expect(wrapper.find('[data-test="moderation-input-legacy"]').exists()).toBe(false)
    await wrapper.get('[data-test="moderation-input-retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-input-legacy"]').text()).toContain('inputLegacyNotice')
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toContain('角色穿上仿生装甲')
    wrapper.unmount()
  })

  it('ignores late responses after switching or closing the input dialog', async () => {
    let finishFirst!: (value: unknown) => void
    getLogInput.mockImplementationOnce(() => new Promise(resolve => { finishFirst = resolve }))
    getLogInput.mockResolvedValueOnce(auditEvidence('第二条审核输入'))
    const wrapper = await mountAudits([auditRow(1), auditRow(2)])
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await wrapper.get('[data-test="moderation-input-2"]').trigger('click')
    await flushPromises()
    finishFirst(auditEvidence('不应出现的旧输入'))
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe('第二条审核输入')
    let finishClosed!: (value: unknown) => void
    getLogInput.mockImplementationOnce(() => new Promise(resolve => { finishClosed = resolve }))
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await findButtonByText(wrapper, 'common.close').trigger('click')
    finishClosed(auditEvidence('关闭后返回'))
    await flushPromises()
    expect(wrapper.find('[data-test="moderation-input-text"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('displays user text and HTTP 200 policy metadata without rendering injected markup', async () => {
    const request = '<img src=x onerror=alert(1)>'.repeat(800) + '请求末尾'
    getLogInput.mockResolvedValue({ ...auditEvidence(''), matched_keyword: '', evidence: {
      text: request, total_characters: request.length, stored_characters: request.length, truncated: false,
      content_scope: 'user_body', source: 'input[2]',
      policy: { code: 'bio_policy', upstream_status: 200 },
    } })
    const wrapper = await mountAudits([{ ...auditRow(), action: 'bio_policy', matched_keyword: '' }])
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe(request)
    expect(wrapper.get('[data-test="moderation-policy-evidence"]').text()).toContain('bio_policy')
    expect(wrapper.get('[data-test="moderation-policy-legacy"]').text()).toContain('policyLegacyExcerpt')
    expect(wrapper.find('[data-test="moderation-policy-response"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="moderation-input-text"] img').exists()).toBe(false)
    expect(wrapper.find('[data-test="moderation-input-retention"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="moderation-input-source"]').text()).toContain('input[2]')
    wrapper.unmount()
  })

  it.each(['cyber_policy', 'bio_policy'])('shows responding pool account and the full %s conversation', async code => {
    const full = JSON.stringify({ instructions: 'system history', input: Array.from({ length: 100 }, (_, i) => ({ role: 'user', content: `message ${i} ` + 'long content '.repeat(300) })) }) + ' FINAL-MARKER'
    getLogInput.mockResolvedValue({ ...auditEvidence(''), matched_keyword: '', evidence: {
      text: full, total_characters: full.length, stored_characters: full.length, truncated: false,
      content_scope: 'full_conversation', source: 'request',
      policy: { code, upstream_status: 200, upstream_body: '<img src=x onerror=alert(1)> upstream error tail' },
    } })
    const wrapper = await mountAudits([{ ...auditRow(), action: code, matched_keyword: '', account_id: 363, account_name: 'upstream-pool-account' }])
    expect(wrapper.get('[data-test="moderation-account-1"]').text()).toContain('upstream-pool-account')
    expect(wrapper.get('[data-test="moderation-account-1"]').text()).toContain('363')
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-detail-account"]').text()).toContain('upstream-pool-account')
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe(full)
    expect(wrapper.get('[data-test="moderation-policy-response"]').text()).toContain('upstream error tail')
    expect(wrapper.find('[data-test="moderation-policy-response"] img').exists()).toBe(false)
    expect(wrapper.find('[data-test="moderation-policy-download"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="moderation-policy-legacy"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('explains missing user text instead of showing tool content or errors as the user body', async () => {
    getLogInput.mockResolvedValue({ input_excerpt: '', matched_keyword: '', evidence: {
      text: '', total_characters: 0, stored_characters: 0, truncated: false, content_scope: 'user_body',
    } })
    const wrapper = await mountAudits([{ ...auditRow(), input_excerpt: '', error: 'upstream noise' }])
    expect(wrapper.get('[data-test="moderation-input-1"]').text()).not.toContain('upstream noise')
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe('admin.riskControl.inputNoUserBody')
    wrapper.unmount()
  })

  it.each(['工具结果 <img src=x> 猫正在画画', ''])('renders separate combination evidence and its source for context %s', async context => {
    getLogInput.mockResolvedValue({ input_excerpt: '用户正文', matched_keyword: '', evidence: {
      text: '用户正文', total_characters: 4, stored_characters: 4, truncated: false, content_scope: 'user_body',
      combination_rules: [{ rule_id: 'test', rule_name: '组合测试', action: 'block', scope: 'conversation',
        window_characters: 400, source: 'input[3]', source_role: 'tool', context,
        matches: [{ group: 'A', keyword: '猫' }, { group: 'B', keyword: '画画' }],
      }],
    } })
    const wrapper = await mountAudits([{ ...auditRow(), action: 'rule_block', matched_keyword: '' }])
    await wrapper.get('[data-test="moderation-input-1"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="moderation-input-text"]').text()).toBe('用户正文')
    const evidence = wrapper.get('[data-test="combination-evidence"]')
    expect(evidence.text()).toContain('sourceRoles.tool')
    expect(evidence.text()).toContain('input[3]')
    expect(evidence.find('img').exists()).toBe(false)
    if (context) {
      expect(evidence.get('[data-test="combination-match-context"]').text()).toBe(context)
      expect(evidence.findAll('mark').map(mark => mark.text())).toEqual(['猫', '画画'])
    } else {
      expect(evidence.text()).toContain('contextOmitted')
      expect(evidence.find('[data-test="combination-match-context"]').exists()).toBe(false)
    }
    wrapper.unmount()
  })

  it.each(Array.from({ length: 8 }, (_, mask) => [Boolean(mask & 1), Boolean(mask & 2), Boolean(mask & 4)]))(
    'saves and restores independent checks api=%s keywords=%s rules=%s', async (api, keywords, rules) => {
      getConfig.mockResolvedValue({ ...baseConfig(), blocked_keywords: ['saved keyword'], rule_filter: { ...defaultRuleFilter(), enabled: true, custom_patterns: [{ name: 'saved-rule', pattern: 'retained-rule-marker', weight: 100 }] } })
      const wrapper = await mountAudits([])
      await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
      await findButtonByText(wrapper, 'admin.riskControl.tabs.keywords').trigger('click')
      const expected = { api, keywords, rules }
      for (const [check, enabled] of Object.entries(expected)) {
        await wrapper.get<HTMLInputElement>(`[data-test="moderation-check-${check}"]`).setValue(enabled)
      }
      expect(wrapper.get<HTMLTextAreaElement>('textarea').element.disabled).toBe(!keywords)
      await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
      await flushPromises()
      expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
        keyword_blocking_mode: api ? (keywords ? 'keyword_and_api' : 'api_only') : (keywords ? 'keyword_only' : 'disabled'),
        blocked_keywords: ['saved keyword'],
        rule_filter: expect.objectContaining({ enabled: rules, custom_patterns: [expect.objectContaining({ name: 'saved-rule' })] }),
      }))
      await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
      await findButtonByText(wrapper, 'admin.riskControl.tabs.keywords').trigger('click')
      for (const [check, enabled] of Object.entries(expected)) {
        expect(wrapper.get<HTMLInputElement>(`[data-test="moderation-check-${check}"]`).element.checked).toBe(enabled)
      }
      if (!api && !keywords && !rules) expect(wrapper.text()).toContain('moderationChecks.none')
      wrapper.unmount()
    }
  )

  it('keeps the combination switch in sync across both settings tabs', async () => {
    const wrapper = await mountAudits([])
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.keywords').trigger('click')
    await wrapper.get<HTMLInputElement>('[data-test="moderation-check-rules"]').setValue(true)
    await findButtonByText(wrapper, 'admin.riskControl.ruleFilter.title').trigger('click')
    const toggle = wrapper.findAllComponents({ name: 'Toggle' }).find(item => item.attributes('data-test') === 'rule-filter-enabled')!
    expect(toggle.props('modelValue')).toBe(true)
    toggle.vm.$emit('update:modelValue', false)
    await findButtonByText(wrapper, 'admin.riskControl.tabs.keywords').trigger('click')
    expect(wrapper.get<HTMLInputElement>('[data-test="moderation-check-rules"]').element.checked).toBe(false)
    expect(wrapper.get<HTMLInputElement>('[data-test="moderation-check-api"]').element.checked).toBe(true)
    expect(wrapper.get<HTMLInputElement>('[data-test="moderation-check-keywords"]').element.checked).toBe(true)
    wrapper.unmount()
  })

  it('saves editable local rules and loads the saved configuration again', async () => {
    getConfig.mockResolvedValue({ ...baseConfig(), rule_filter: { ...defaultRuleFilter(), enabled: true, custom_patterns: [{ name: 'Editable rule', pattern: 'local-sentinel-marker', weight: 100 }] } })
    const wrapper = await mountAudits([])
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.ruleFilter.title').trigger('click')
    await wrapper.get('[data-test="rule-name"]').setValue('Renamed rule')
    await wrapper.get('[data-test="rule-threshold"]').setValue('60')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ rule_filter: expect.objectContaining({ threshold: 60, custom_patterns: [expect.objectContaining({ name: 'Renamed rule' })] }) }))
    expect(showError).not.toHaveBeenCalled()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.ruleFilter.title').trigger('click')
    expect(wrapper.get<HTMLInputElement>('[data-test="rule-name"]').element.value).toBe('Renamed rule')
    wrapper.unmount()
  })

  it.each([undefined, 1600])('loads and saves keyword context with existing value %s', async (configured) => {
    getConfig.mockResolvedValue({ ...baseConfig(), keyword_context_chars: configured })
    updateConfig.mockImplementation(async payload => ({ ...baseConfig(), ...payload }))
    const wrapper = await mountAudits([])
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.retention').trigger('click')
    const input = wrapper.get<HTMLInputElement>('[data-test="moderation-keyword-context-chars"]')
    expect(input.element.value).toBe(String(configured ?? 640))
    await input.setValue('2400')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ keyword_context_chars: 2400 }))
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.retention').trigger('click')
    expect(wrapper.get<HTMLInputElement>('[data-test="moderation-keyword-context-chars"]').element.value).toBe('2400')
    wrapper.unmount()
  })

  it.each(['', '0', '-1', '12001', '1.5'])('rejects invalid keyword context %s without saving', async value => {
    const wrapper = await mountAudits([])
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.retention').trigger('click')
    await wrapper.get('[data-test="moderation-keyword-context-chars"]').setValue(value)
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.riskControl.keywordContextCharsInvalid')
    wrapper.unmount()
  })

  it('loads and saves the shared retention capacity', async () => {
    getConfig.mockResolvedValue({ ...baseConfig(), log_storage_limit_gb: 50 })
    const wrapper = await mountAudits([])
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.retention').trigger('click')
    const capacity = wrapper.get<HTMLInputElement>('[data-test="moderation-log-capacity"]')
    expect(capacity.element.value).toBe('50')
    expect(capacity.attributes('max')).toBe('200')
    await capacity.setValue('25')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ log_storage_limit_gb: 25 }))
    wrapper.unmount()
  })

  it.each(['openai', 'typesafe'] as const)('shows the selected draft engine keys while %s remains active', async (activeEngine) => {
    const openaiKey: ContentModerationAPIKeyStatus = {
      index: 1, key_hash: 'openai-hash', masked: '********oa01', status: 'frozen',
      failure_count: 1, success_count: 0, last_error: '', last_latency_ms: 100,
      last_http_status: 429, last_tested: false, configured: true,
    }
    const typesafeKey: ContentModerationAPIKeyStatus = { ...openaiKey, key_hash: 'typesafe-hash', masked: '********ts02', status: 'ok', failure_count: 0, success_count: 1, last_http_status: 200 }
    const profiles = {
      openai: { ...baseConfig(), engine: 'openai' as const, api_key_configured: true, api_key_count: 1, api_key_statuses: [openaiKey] },
      typesafe: { ...baseConfig(), engine: 'typesafe' as const, api_key_configured: true, api_key_count: 1, api_key_statuses: [typesafeKey] },
    }
    const otherEngine = activeEngine === 'openai' ? 'typesafe' : 'openai'
    getConfig.mockResolvedValue({ ...profiles[activeEngine], engine_configs: profiles })
    getStatus.mockResolvedValue({ ...runtimeStatus(), engine: activeEngine, api_key_statuses: profiles[activeEngine].api_key_statuses })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    const engine = () => wrapper.findComponent('[data-test="audit-engine-select"]')
    const keyPanel = () => wrapper.get('[data-test="audit-key-statuses"]')
    expect(keyPanel().text()).toContain(profiles[activeEngine].api_key_statuses[0].masked)
    engine().vm.$emit('update:modelValue', otherEngine)
    await flushPromises()
    expect(keyPanel().text()).toContain(profiles[otherEngine].api_key_statuses[0].masked)
    expect(keyPanel().text()).not.toContain(profiles[activeEngine].api_key_statuses[0].masked)
    expect(keyPanel().text()).toContain(otherEngine === 'openai' ? 'admin.riskControl.apiKeyStatusFrozen' : 'admin.riskControl.apiKeyStatusOk')
    await findButtonByText(wrapper, 'admin.riskControl.refreshStatus').trigger('click')
    await flushPromises()
    expect(keyPanel().text()).toContain(profiles[otherEngine].api_key_statuses[0].masked)
    expect(keyPanel().text()).not.toContain(profiles[activeEngine].api_key_statuses[0].masked)
    engine().vm.$emit('update:modelValue', activeEngine)
    await flushPromises()
    expect(keyPanel().text()).toContain(profiles[activeEngine].api_key_statuses[0].masked)
    expect(keyPanel().text()).not.toContain(profiles[otherEngine].api_key_statuses[0].masked)
    expect(updateConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('assigns an existing key without saving drafts, then displays its dedicated IPv6', async () => {
    const key: ContentModerationAPIKeyStatus = {
      index: 0, key_hash: 'a'.repeat(64), masked: '********abcd', status: 'ok', failure_count: 0,
      success_count: 1, last_error: '', last_latency_ms: 120, last_http_status: 200, last_tested: true, configured: true,
    }
    const config = { ...baseConfig(), engine: 'openai' as const, api_key_count: 1, api_key_configured: true, api_key_statuses: [key] }
    getConfig.mockResolvedValue(config)
    getStatus.mockResolvedValue({ ...runtimeStatus(), api_key_statuses: [key] })
    let complete!: (value: ContentModerationConfig) => void
    allocateAPIKeyIPv6.mockImplementation(() => new Promise(resolve => { complete = resolve }))
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await wrapper.get('[data-test="audit-model"]').setValue('unsaved-model')
    let finishStaleStatus!: (value: unknown) => void
    getStatus.mockImplementationOnce(() => new Promise(resolve => { finishStaleStatus = resolve }))
    await findButtonByText(wrapper, 'admin.riskControl.refreshStatus').trigger('click')
    const button = wrapper.get<HTMLButtonElement>('[data-allocate-ipv6]')
    await button.trigger('click')
    expect(button.element.disabled).toBe(true)
    expect(allocateAPIKeyIPv6).toHaveBeenCalledTimes(1)
    expect(allocateAPIKeyIPv6).toHaveBeenCalledWith('openai', key.key_hash)
    complete({ ...config, api_key_statuses: [{ ...key, managed_ipv6: '2001:db8::a', proxy_id: 42 }] })
    await flushPromises()
    finishStaleStatus({ ...runtimeStatus(), api_key_statuses: [key] })
    await flushPromises()
    expect(wrapper.get('[data-test="audit-key-ipv6"]').text()).toContain('2001:db8::a')
    expect(wrapper.find('[data-allocate-ipv6]').exists()).toBe(false)
    expect((wrapper.get('[data-test="audit-model"]').element as HTMLInputElement).value).toBe('unsaved-model')
    expect(updateConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('saves automatic IPv6 independently per engine and prevents testing unsaved keys', async () => {
    getConfig.mockResolvedValue({ ...baseConfig(), engine: 'openai', engine_configs: { openai: baseConfig(), typesafe: baseConfig() } })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await wrapper.get('[data-test="audit-auto-ipv6"]').setValue(true)
    await wrapper.get('textarea[autocomplete="new-password"]').setValue('new-key')
    expect(findButtonByText(wrapper, 'admin.riskControl.testInputApiKeys').element.disabled).toBe(true)
    wrapper.findComponent('[data-test="audit-engine-select"]').vm.$emit('update:modelValue', 'typesafe')
    await flushPromises()
    expect((wrapper.get('[data-test="audit-auto-ipv6"]').element as HTMLInputElement).checked).toBe(false)
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig.mock.calls[0]![0].engine_configs).toMatchObject({
      openai: { auto_assign_ipv6: true, api_keys: ['new-key'] }, typesafe: { auto_assign_ipv6: false },
    })
    wrapper.unmount()
  })

  it('refreshes committed bindings after an allocation timeout', async () => {
    const key: ContentModerationAPIKeyStatus = { index:0, key_hash:'b'.repeat(64), masked:'********efgh', status:'unknown', failure_count:0, success_count:0, last_error:'', last_latency_ms:0, last_http_status:0, last_tested:false, configured:true }
    const config = { ...baseConfig(), engine:'openai' as const, api_key_count:1, api_key_configured:true, api_key_statuses:[key] }
    getConfig.mockResolvedValueOnce(config).mockResolvedValue({ ...config, api_key_statuses:[{ ...key, managed_ipv6:'2001:db8::b', proxy_id:43 }] })
    getStatus.mockResolvedValue({ ...runtimeStatus(), api_key_statuses:[key] })
    allocateAPIKeyIPv6.mockRejectedValue(new Error('timeout'))
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon:true, Select:true, Toggle:true, Pagination:true, ModelWhitelistSelector:ModelWhitelistSelectorStub, ProxySelector:true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await wrapper.get('[data-allocate-ipv6]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="audit-key-ipv6"]').text()).toContain('2001:db8::b')
    expect(allocateAPIKeyIPv6).toHaveBeenCalledTimes(1)
    expect(updateConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('renders image safety rejections as log-only hits, not audit errors', async () => {
    const row: ContentModerationLog = {
      engine_meta: null, id: 1, request_id: 'req-1', user_id: 100, user_email: 'u@example.com',
      api_key_id: 9, api_key_name: 'key', group_id: 7, group_name: 'img', endpoint: '/v1/images/generations',
      provider: 'openai', model: 'gpt-image-2', mode: 'risk_control_log_only', action: 'image_safety', flagged: true,
      highest_category: 'image_safety', highest_score: 1, matched_keyword: '', category_scores: {}, threshold_snapshot: {},
      input_excerpt: 'draw something', upstream_latency_ms: null,
      error: 'upstream_status=400\nYour request was rejected by the safety system.',
      violation_count: 0, auto_banned: false, email_sent: false, user_status: 'active', queue_delay_ms: null,
      created_at: '2026-11-27T00:00:00Z',
    }
    listLogs.mockResolvedValue({ items: [row], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()

    const badge = wrapper.findAll('span.rounded-md').find((el) => el.text() === 'admin.riskControl.action.imageSafety')
    expect(badge).toBeDefined()
    expect(badge!.classes()).toContain('bg-pink-100')
    expect(badge!.classes()).not.toContain('bg-amber-100')
  })

  it('shows an empty key panel for an unconfigured draft engine', async () => {
    getStatus.mockResolvedValue({ ...runtimeStatus(), api_key_statuses: [{ masked: '********oa01', configured: true, status: 'ok' }] })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    wrapper.findComponent('[data-test="audit-engine-select"]').vm.$emit('update:modelValue', 'typesafe')
    await flushPromises()
    expect(wrapper.get('[data-test="audit-key-statuses"]').text()).toContain('admin.riskControl.apiKeyHealthEmpty')
    expect(wrapper.get('[data-test="audit-key-statuses"]').text()).not.toContain('********oa01')
    expect(updateConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('preserves both engine drafts and saves thresholds independently', async () => {
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await wrapper.get('[data-test="audit-base-url"]').setValue('https://openai-edited.example')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.riskThresholds').trigger('click')
    await wrapper.get('[data-test="risk-threshold-sexual"]').setValue('73')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.basic').trigger('click')
    const engine = () => wrapper.findComponent('[data-test="audit-engine-select"]')
    engine().vm.$emit('update:modelValue', 'typesafe')
    await flushPromises()
    expect((wrapper.get('[data-test="audit-model"]').element as HTMLInputElement).value).toBe('jev-latest')
    await wrapper.get('[data-test="audit-base-url"]').setValue('https://typesafe-edited.example')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.riskThresholds').trigger('click')
    expect((wrapper.get('[data-test="risk-threshold-sexual"]').element as HTMLInputElement).value).toBe('65')
    await wrapper.get('[data-test="risk-threshold-sexual"]').setValue('91')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.basic').trigger('click')
    engine().vm.$emit('update:modelValue', 'openai')
    await flushPromises()
    expect((wrapper.get('[data-test="audit-base-url"]').element as HTMLInputElement).value).toBe('https://openai-edited.example')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ engine: 'openai', engine_configs: {
      openai: expect.objectContaining({ base_url: 'https://openai-edited.example', thresholds: expect.objectContaining({ sexual: 0.73 }) }),
      typesafe: expect.objectContaining({ base_url: 'https://typesafe-edited.example', thresholds: expect.objectContaining({ sexual: 0.91 }) }),
    } }))
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('tests the draft engine without switching the active engine', async () => {
    testAPIKeys.mockResolvedValue({ items: [{ status: 'ok', configured: false }], image_count: 0 })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    wrapper.findComponent('[data-test="audit-engine-select"]').vm.$emit('update:modelValue', 'typesafe')
    await flushPromises()
    await wrapper.get('textarea[autocomplete="new-password"]').setValue('test-only-key')
    await findButtonByText(wrapper, 'admin.riskControl.testInputApiKeys').trigger('click')
    await flushPromises()
    expect(testAPIKeys).toHaveBeenCalledWith(expect.objectContaining({ engine: 'typesafe', api_keys: ['test-only-key'], model: 'jev-latest', thresholds: expect.objectContaining({ sexual: 0.65 }) }))
    expect(updateConfig).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="active-audit-engine"]').text()).not.toContain('TypeSafe AI')
    wrapper.unmount()
  })

  it.each(['openai', 'typesafe'] as const)('restores the shared defaults for %s without replacing saved thresholds on load', async (engine) => {
    getConfig.mockResolvedValue({ ...baseConfig(), engine, thresholds: { sexual: 0.8, harassment: 0.8 } })
    const wrapper = mount(RiskControlView, { global: { stubs: { AppLayout: AppLayoutStub, BaseDialog: BaseDialogStub, Icon: true, Select: true, Toggle: true, Pagination: true, ModelWhitelistSelector: ModelWhitelistSelectorStub, ProxySelector: true } } })
    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.riskThresholds').trigger('click')
    expect((wrapper.get('[data-test="risk-threshold-sexual"]').element as HTMLInputElement).value).toBe('80')
    await findButtonByText(wrapper, 'admin.riskControl.riskThresholdReset').trigger('click')
    const defaults = {
      harassment: 98, 'harassment/threatening': 90, hate: 65, 'hate/threatening': 65,
      illicit: 95, 'illicit/violent': 95, 'self-harm': 65, 'self-harm/intent': 85,
      'self-harm/instructions': 65, sexual: 65, 'sexual/minors': 65, violence: 95, 'violence/graphic': 95,
    }
    for (const [category, value] of Object.entries(defaults)) {
      expect((wrapper.get(`[data-test="risk-threshold-${category}"]`).element as HTMLInputElement).value).toBe(String(value))
    }
    if (engine === 'typesafe') expect(wrapper.text()).toContain('admin.riskControl.typeSafeThresholds')
    expect(updateConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('saves the selected model filter mode and models', async () => {
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.scope').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.modelFilterInclude').trigger('click')
    await wrapper.get('[data-test="model-filter-input"]').setValue('gpt-5.5, gpt-5.4')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      model_filter: {
        type: 'include',
        models: ['gpt-5.5', 'gpt-5.4'],
      },
    }))
    expect(showError).not.toHaveBeenCalled()
  })

  it('submits edited risk control thresholds when saving moderation config', async () => {
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.riskThresholds').trigger('click')
    await wrapper.get('[data-test="risk-threshold-sexual"]').setValue('72')
    await wrapper.get('[data-test="risk-threshold-harassment"]').setValue('99')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      thresholds: expect.objectContaining({
        sexual: 0.72,
        harassment: 0.99,
      }),
    }))
    expect(showError).not.toHaveBeenCalled()
  })

  it('describes worker runtime as async audit and pre-block record processing', async () => {
    getStatus.mockResolvedValue({
      ...runtimeStatus(),
      mode: 'observe',
      processed: 12,
      queue_length: 2,
    })

    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.riskControl.workerStatusHint')
    expect(wrapper.text()).not.toContain('admin.riskControl.preBlockSyncStatus')
    expect(wrapper.text()).toContain('admin.riskControl.records')
    expect(wrapper.text()).toContain('12')
    expect(wrapper.text()).toContain('2 / 32,768')
  })

  it('shows pre-block synchronous moderation metrics separately from worker queue', async () => {
    getStatus.mockResolvedValue({
      ...runtimeStatus(),
      pre_block_active: 2,
      pre_block_checked: 128,
      pre_block_allowed: 120,
      pre_block_blocked: 8,
      pre_block_errors: 1,
      pre_block_avg_latency_ms: 86,
      pre_block_api_key_active: 2,
      pre_block_api_key_available_count: 2,
      pre_block_api_key_total_calls: 128,
      active_workers: 3,
      worker_count: 7,
      pre_block_api_key_loads: [
        {
          index: 0,
          key_hash: 'hash-one',
          masked: 'sk-...one',
          status: 'ok',
          active: 1,
          total: 72,
          success: 70,
          errors: 2,
          avg_latency_ms: 84,
          last_latency_ms: 80,
          last_http_status: 200,
        },
        {
          index: 1,
          key_hash: 'hash-two',
          masked: 'sk-...two',
          status: 'ok',
          active: 1,
          total: 56,
          success: 56,
          errors: 0,
          avg_latency_ms: 90,
          last_latency_ms: 92,
          last_http_status: 200,
        },
      ],
    })

    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.riskControl.preBlockSyncStatus')
    expect(wrapper.text()).toContain('admin.riskControl.preBlockSyncHint')
    expect(wrapper.text()).not.toContain('admin.riskControl.workerStatus')
    expect(wrapper.text()).toContain('admin.riskControl.records')
    expect(wrapper.text()).toContain('128')
    expect(wrapper.text()).toContain('120')
    expect(wrapper.text()).toContain('8')
    expect(wrapper.text()).toContain('86 ms')
    expect(wrapper.text()).toContain('admin.riskControl.preBlockAPIKeyLoad')
    expect(wrapper.text()).toContain('sk-...one')
    expect(wrapper.text()).toContain('sk-...two')
    expect(wrapper.text()).toContain('72')
    expect(wrapper.text()).toContain('56')
    expect(wrapper.text()).toContain('同步并发 2 / 可用 Key 2，累计 128 次，worker：3 / 7')

    const runtimeCards = wrapper.get('[data-test="pre-block-runtime-cards"]')
    const syncCard = wrapper.get('[data-test="pre-block-sync-card"]')
    const apiKeyLoadCard = wrapper.get('[data-test="pre-block-api-key-load-card"]')

    expect(runtimeCards.classes()).toEqual(expect.arrayContaining([
      'grid',
      'grid-cols-1',
      'xl:grid-cols-[minmax(0,520px)_minmax(0,1fr)]',
    ]))
    expect(syncCard.element.parentElement).toBe(runtimeCards.element)
    expect(apiKeyLoadCard.element.parentElement).toBe(runtimeCards.element)
    expect(syncCard.classes()).toContain('card')
    expect(apiKeyLoadCard.classes()).toContain('card')
    expect(syncCard.get('h2').text()).toBe('admin.riskControl.preBlockSyncStatus')
    expect(syncCard.text()).toContain('admin.riskControl.preBlockSyncHint')
    expect(apiKeyLoadCard.get('h2').text()).toBe('admin.riskControl.preBlockAPIKeyLoad')
    expect(apiKeyLoadCard.text()).toContain('admin.riskControl.preBlockAPIKeyLoadHint')
    expect(wrapper.get('[data-test="pre-block-api-key-load-list"]').classes()).toEqual(expect.arrayContaining([
      'max-h-[280px]',
      'overflow-y-auto',
    ]))
  })
})
