import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'

const { listByAccount, create, update } = vi.hoisted(() => ({ listByAccount: vi.fn(), create: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { scheduledTests: { listByAccount, create, update } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', async (original) => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)

const plan = { id: 8, account_id: 71, model_id: 'gpt-5.4', prompt: 'saved message', reasoning_effort: 'high', cron_expression: '*/5 * * * *', enabled: false, auto_recover: false, max_results: 10 }
async function openPanel() {
  const w = mount(ScheduledTestsPanel, {
    props: { show: false, accountId: 71, accountPlatform: 'openai', modelOptions: [{ value: 'gpt-5.4', label: 'GPT-5.4' }] },
    global: { stubs: {
      BaseDialog: { template: '<div><slot /></div>' }, ConfirmDialog: true, Icon: true, HelpTooltip: true,
      Select: { props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option value=""></option><option v-for="o in options" :key="o.value" :value="o.value">{{o.label}}</option></select>' }
    } }
  })
  await w.setProps({ show: true }); await flushPromises()
  return w
}

describe('scheduled test request parameters', () => {
  beforeEach(() => {
    vi.clearAllMocks(); listByAccount.mockResolvedValue([plan]); create.mockResolvedValue(plan); update.mockImplementation(async (_id, body) => ({ ...plan, ...body }))
  })

  it('creates a plan using the message and reasoning selected in the form', async () => {
    const w = await openPanel()
    await w.findAll('button').find(b => b.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    await w.find('select').setValue('gpt-5.4')
    await w.find('input[placeholder="*/30 * * * *"]').setValue('*/5 * * * *')
    await w.find('#scheduled-new-prompt').setValue('  定时测试\n只回复 21  ')
    await w.find('#scheduled-new-effort').setValue('xhigh')
    await w.findAll('button').find(b => b.text().includes('common.save'))!.trigger('click')
    await flushPromises()
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ account_id: 71, prompt: '  定时测试\n只回复 21  ', reasoning_effort: 'xhigh', cron_expression: '*/5 * * * *', auto_recover: false }))
  })

  it('loads saved values and allows explicitly clearing both without enabling recovery', async () => {
    const w = await openPanel()
    await w.find('button[title="admin.scheduledTests.editPlan"]').trigger('click')
    expect((w.find('#scheduled-edit-8-prompt').element as HTMLTextAreaElement).value).toBe('saved message')
    expect((w.find('#scheduled-edit-8-effort').element as HTMLSelectElement).value).toBe('high')
    await w.find('#scheduled-edit-8-prompt').setValue('')
    await w.find('#scheduled-edit-8-effort').setValue('')
    await w.findAll('button').find(b => b.text().includes('common.save'))!.trigger('click')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(8, expect.objectContaining({ prompt: '', reasoning_effort: '', enabled: false, auto_recover: false }))
  })

  it('does not leak an unsaved message into another account', async () => {
    const w = await openPanel()
    await w.findAll('button').find(b => b.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    await w.find('#scheduled-new-prompt').setValue('private to first account')
    await w.setProps({ show: false }); await w.setProps({ show: true, accountId: 72 }); await flushPromises()
    await w.findAll('button').find(b => b.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    expect((w.find('#scheduled-new-prompt').element as HTMLTextAreaElement).value).toBe('')
  })
})
