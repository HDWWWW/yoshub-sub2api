import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRiskDetection from '../AccountRiskDetection.vue'
import type { AccountRiskRule } from '@/api/admin/accountRisk'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountRisk', () => ({ accountRiskAPI: { get, save } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const baseline: AccountRiskRule = {
  group_id: 7, revision: 3,
  config: { enabled: false, notify_enabled: false, first_below_percent: 60, second_at_least_percent: 90, sustain_seconds: 10, probe_interval_seconds: 300, probe_prompt: 'Reply 21', expected_text: '21', model: 'gpt-6.1-sol', reasoning_effort: 'high' },
  observation: { checked_at: '', mode: 'disabled', message: 'off', eligible_accounts: 0, input_tokens: 0, output_tokens: 0 }, events: []
}
const groups = [{ id: 7, name: 'OpenAI A', platform: 'openai' }, { id: 8, name: 'OpenAI B', platform: 'openai' }, { id: 9, name: 'Claude', platform: 'anthropic' }]
async function open() {
  const w = mount(AccountRiskDetection, { props: { groups, initialGroup: '7' }, global: { stubs: { Icon: true, BaseDialog: { template: '<div><slot /></div>' } } } })
  await w.get('[data-testid="open-risk-detection"]').trigger('click'); await flushPromises(); return w
}
describe('account risk detection', () => {
  beforeEach(() => { vi.resetAllMocks(); get.mockImplementation(async () => structuredClone(baseline)); save.mockImplementation(async (group_id, revision, config) => ({ ...structuredClone(baseline), group_id, revision: revision + 1, config })) })
  it('loads selected OpenAI group and saves every custom control with revision', async () => {
    const w = await open()
    expect(get).toHaveBeenCalledWith(7)
    expect(w.get('[data-testid="risk-group"]').text()).not.toContain('Claude')
    await w.get('[data-testid="risk-enabled"]').setValue(true)
    await w.get('[data-testid="risk-notify-enabled"]').setValue(true)
    for (const [field, value] of Object.entries({ first: 55, second: 95, duration: 17, interval: 420, model: 'gpt-6-astra', effort: 'xhigh', prompt: 'Compute three times seven', expected: '21' })) await w.get(`[data-testid="risk-${field}"]`).setValue(value)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save).toHaveBeenCalledWith(7, 3, { enabled: true, notify_enabled: true, first_below_percent: 55, second_at_least_percent: 95, sustain_seconds: 17, probe_interval_seconds: 420, model: 'gpt-6-astra', reasoning_effort: 'xhigh', probe_prompt: 'Compute three times seven', expected_text: '21' })
    expect(w.get('[role="status"]').text()).toContain('saved'); w.unmount()
  })
  it.each([['first', 0], ['second', 101], ['duration', 0], ['interval', 9], ['prompt', ''], ['expected', ''], ['model', '']])('blocks invalid %s', async (field, value) => {
    const w = await open(); await w.get(`[data-testid="risk-${field}"]`).setValue(value)
    await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled(); w.unmount()
  })
  it('does not offer saving when loading fails', async () => {
    get.mockRejectedValue(new Error('offline')); const w = await open()
    expect(w.find('form').exists()).toBe(false); expect(w.find('[role="alert"]').exists()).toBe(true); w.unmount()
  })
  it('discards a stale group response', async () => {
    const w = await open()
    let resolveOld!: (value: AccountRiskRule) => void
    get.mockImplementationOnce(() => new Promise<AccountRiskRule>(resolve => { resolveOld = resolve }))
    await w.get('[data-testid="risk-group"]').setValue(8)
    get.mockResolvedValueOnce({ ...structuredClone(baseline), group_id: 7, revision: 12 })
    await w.get('[data-testid="risk-group"]').setValue(7); await flushPromises()
    resolveOld({ ...structuredClone(baseline), group_id: 8, revision: 99 }); await flushPromises()
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.slice(0, 2)).toEqual([7, 12]); w.unmount()
  })
  it('shows conflict without silently overwriting settings', async () => {
    const w = await open(); save.mockRejectedValue(new Error('conflict'))
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save).toHaveBeenCalledTimes(1); expect(w.find('[role="alert"]').exists()).toBe(true); w.unmount()
  })
  it('refreshes actual enabled state while preserving the draft and revision', async () => {
    vi.useFakeTimers()
    const w = await open()
    await w.get('[data-testid="risk-prompt"]').setValue('unsaved custom message')
    get.mockResolvedValue({ ...structuredClone(baseline), revision: 4, config: { ...baseline.config, enabled: true } })
    await vi.advanceTimersByTimeAsync(5000); await flushPromises()
    expect(w.text()).toContain('accountRisk.active')
    expect((w.get('[data-testid="risk-enabled"]').element as HTMLInputElement).checked).toBe(false)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.[1]).toBe(3)
    expect(save.mock.calls[0]?.[2].probe_prompt).toBe('unsaved custom message')
    w.unmount(); vi.useRealTimers()
  })
  it.each(['ready', 'missing', 'unavailable'] as const)('shows robot status %s without exposing a connection', async status => {
    get.mockResolvedValue({ ...structuredClone(baseline), webhook_status: status, events: [{ at: new Date().toISOString(), account_id: 1, reason: 'confirmed stop', notification: 'failed' }] })
    const w = await open()
    expect(w.get('[data-testid="risk-webhook-status"]').text()).toContain(`accountRisk.webhook.${status}`)
    expect(w.get('[data-testid="risk-notification-result"]').text()).toContain('accountRisk.notification.failed')
    expect((w.get('[data-testid="risk-notify-enabled"]').element as HTMLInputElement).checked).toBe(false)
    w.unmount()
  })
  it('defaults a legacy missing notification setting to off', async () => {
    const old = structuredClone(baseline)
    Reflect.deleteProperty(old.config, 'notify_enabled'); get.mockResolvedValue(old)
    const w = await open(); await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.[2].notify_enabled).toBe(false); w.unmount()
  })
  it('a stale refresh cannot overwrite a newly saved notification setting', async () => {
    vi.useFakeTimers(); const w = await open()
    let resolveOld!: (value: AccountRiskRule) => void
    get.mockImplementationOnce(() => new Promise<AccountRiskRule>(resolve => { resolveOld = resolve }))
    await vi.advanceTimersByTimeAsync(5000)
    await w.get('[data-testid="risk-notify-enabled"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    resolveOld(structuredClone(baseline)); await flushPromises()
    expect((w.get('[data-testid="risk-notify-enabled"]').element as HTMLInputElement).checked).toBe(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[1]?.[1]).toBe(4)
    w.unmount(); vi.useRealTimers()
  })
})
