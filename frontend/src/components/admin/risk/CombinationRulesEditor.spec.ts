import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import CombinationRulesEditor from './CombinationRulesEditor.vue'
import type { CombinationRule } from '@/api/admin/riskControl'

const { testRules } = vi.hoisted(() => ({ testRules: vi.fn() }))
vi.mock('@/api/admin/riskControl', () => ({ testCombinationRules: testRules }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const rule = (): CombinationRule => ({ id: 'custom', name: 'Custom', enabled: true, action: 'block', scope: 'conversation', window_characters: 400, groups: [{ name: 'A', keywords: ['猫'] }, { name: 'B', keywords: ['画'] }] })
function editor(rules: CombinationRule[] = []) {
  const wrapper = mount(CombinationRulesEditor, { props: { modelValue: rules, 'onUpdate:modelValue': value => wrapper.setProps({ modelValue: value }) } })
  return wrapper
}
beforeEach(() => testRules.mockReset())

describe('CombinationRulesEditor', () => {
  it('adds, edits and removes independent rules and groups without losing newline input', async () => {
    const wrapper = editor()
    await wrapper.get('[data-test="add-rule"]').trigger('click')
    await wrapper.get('[data-test="rule-name"]').setValue('A new topic')
    await wrapper.get('[data-test="rule-window"]').setValue('650')
    await wrapper.get('[data-test="rule-action"]').setValue('block')
    await wrapper.get('[data-test="add-group"]').trigger('click')
    expect(wrapper.findAll('[data-test="rule-group"]')).toHaveLength(3)
    await wrapper.findAll('[data-test="group-words"]')[0].setValue('first\nsecond\n')
    expect(wrapper.props('modelValue')[0]).toMatchObject({ name: 'A new topic', window_characters: 650, action: 'block' })
    expect(wrapper.props('modelValue')[0].groups[0].keywords).toEqual(['first', 'second', ''])
    await wrapper.get('[role="switch"]').trigger('click')
    expect(wrapper.props('modelValue')[0].enabled).toBe(false)
    await wrapper.get('[data-test="remove-rule"]').trigger('click')
    expect(wrapper.props('modelValue')).toEqual([])
  })

  it('tests the edited draft and discards stale results after further edits', async () => {
    const wrapper = editor([rule()])
    await wrapper.get('[data-test="rule-name"]').setValue('Edited draft')
    await wrapper.get('[data-test="rule-test-text"]').setValue('请画一只猫')
    testRules.mockResolvedValue({ matches: [{ rule_id: 'custom', rule_name: 'Edited draft', action: 'block', matches: [{ group: 'A', keyword: '猫' }], context: '<script>unsafe</script>' }] })
    await wrapper.get('[data-test="test-rules"]').trigger('click')
    await flushPromises()
    expect(testRules).toHaveBeenCalledWith(expect.arrayContaining([expect.objectContaining({ name: 'Edited draft' })]), '请画一只猫')
    expect(wrapper.get('[data-test="rule-test-result"]').text()).toContain('<script>unsafe</script>')
    expect(wrapper.find('script').exists()).toBe(false)
    await wrapper.get('[data-test="rule-window"]').setValue('500')
    expect(wrapper.find('[data-test="rule-test-result"]').exists()).toBe(false)
  })
})
