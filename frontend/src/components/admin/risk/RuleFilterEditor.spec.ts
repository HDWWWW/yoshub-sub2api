import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import RuleFilterEditor from './RuleFilterEditor.vue'
import RuleFilterEvidenceView from './RuleFilterEvidence.vue'
import { defaultRuleFilter } from '@/api/admin/riskControl'
import type { RuleFilterConfig, RuleFilterEvidence } from '@/api/admin/riskControl'

const { getCatalog, testRules } = vi.hoisted(() => ({ getCatalog: vi.fn(), testRules: vi.fn() }))
vi.mock('@/api/admin/riskControl', async (original) => ({ ...(await original<typeof import('@/api/admin/riskControl')>()), getRuleFilterCatalog: getCatalog, testRuleFilter: testRules }))
const plugins = () => [createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })]
const evidence = (): RuleFilterEvidence => ({ version: 'test', action: 'block', would_action: 'block', score: 100, raw_score: 100, audit_score: 100, threshold: 50, strict_threshold: 90, reason: '', truncated: false, deferred: false, signals: [{ origin: 'current_user', layer_mode: 'enforce', score: 100, raw_score: 100, action: 'block', matches: [{ name: 'custom', weight: 100 }], context: '<img src=x onerror=alert(1)> local-marker', keywords: ['local-marker'] }] })

beforeEach(() => {
  vi.clearAllMocks()
  getCatalog.mockResolvedValue({ version: 'test', defaults: defaultRuleFilter(), presets: [{ name: 'preset-a', pattern: 'preset-regex', weight: 100, category: 'test' }] })
  testRules.mockResolvedValue(evidence())
})

describe('Rule filter controls', () => {
  it('disables presets, edits custom patterns and restores defaults without removing custom patterns', async () => {
    const config = { ...defaultRuleFilter(), enabled: true, custom_patterns: [{ name: 'local', pattern: 'old-marker', weight: 100 }] }
    const wrapper = mount(RuleFilterEditor, { props: { modelValue: config, 'onUpdate:modelValue': (v: RuleFilterConfig) => wrapper.setProps({ modelValue: v }) }, global: { plugins: plugins() } })
    await flushPromises()
    const toggle = wrapper.get('[data-test="preset-rule"]').getComponent({ name: 'Toggle' })
    toggle.vm.$emit('update:modelValue', false)
    await flushPromises()
    expect(wrapper.props('modelValue').disabled_patterns).toEqual(['preset-a'])
    await wrapper.get('[data-test="rule-pattern"]').setValue('new-marker')
    await wrapper.get('[data-test="restore-presets"]').trigger('click')
    expect(wrapper.props('modelValue').disabled_patterns).toEqual([])
    expect(wrapper.props('modelValue').custom_patterns[0].pattern).toBe('new-marker')
    expect(wrapper.props('modelValue').enabled).toBe(true)
    wrapper.unmount()
  })

  it('previews the draft and invalidates stale results after edits', async () => {
    const config = { ...defaultRuleFilter(), enabled: true }
    const wrapper = mount(RuleFilterEditor, { props: { modelValue: config, 'onUpdate:modelValue': (v: RuleFilterConfig) => wrapper.setProps({ modelValue: v }) }, global: { plugins: plugins() } })
    await wrapper.get('[data-test="rule-test-text"]').setValue('local-marker')
    await wrapper.get('[data-test="test-rules"]').trigger('click'); await flushPromises()
    expect(testRules).toHaveBeenCalledWith(config, 'local-marker')
    expect(wrapper.find('[data-test="rule-filter-evidence"]').exists()).toBe(true)
    await wrapper.get('[data-test="rule-test-text"]').setValue('changed')
    expect(wrapper.find('[data-test="rule-filter-evidence"]').exists()).toBe(false)
    let resolve!: (value: RuleFilterEvidence) => void
    testRules.mockImplementationOnce(() => new Promise(r => { resolve = r }))
    await wrapper.get('[data-test="test-rules"]').trigger('click')
    await wrapper.get('[data-test="rule-test-text"]').setValue('newer')
    resolve(evidence()); await flushPromises()
    expect(wrapper.find('[data-test="rule-filter-evidence"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('renders source evidence and highlights without interpreting HTML', () => {
    const wrapper = mount(RuleFilterEvidenceView, { props: { evidence: evidence() }, global: { plugins: plugins() } })
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.get('mark').text()).toBe('local-marker')
    expect(wrapper.text()).toContain('current_user')
    expect(wrapper.text()).toContain('100 / 50')
    expect(wrapper.text()).not.toContain('100%')
  })
})
