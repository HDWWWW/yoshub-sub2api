import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountAutoFast from '../AccountAutoFast.vue'
import type { AccountAutoFastRule } from '@/api/admin/accountAutoFast'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountAutoFast', () => ({ accountAutoFastAPI: { get, save } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
const baseline: AccountAutoFastRule = {
  group_id: 7, revision: 3,
  config: { enabled: false, require_multiple_accounts: false, stages: [{ minutes: 45, used_below_percent: 50 }, { minutes: 30, used_below_percent: 65 }, { minutes: 15, used_below_percent: 80 }], final_minutes: 5, usage_max_age_seconds: 300 },
  checked_at: '', total: 0, eligible_accounts: 0, accounts: [], events: []
}
const groups = [{ id: 7, name: 'OpenAI A', platform: 'openai' }, { id: 8, name: 'OpenAI B', platform: 'openai' }, { id: 9, name: 'Claude', platform: 'anthropic' }]
async function open() {
  const w = mount(AccountAutoFast, { props: { groups, initialGroup: '7' }, global: { stubs: { Icon: true, BaseDialog: { template: '<div><slot /></div>' } } } })
  await w.get('[data-testid="open-auto-fast"]').trigger('click'); await flushPromises(); return w
}

describe('account auto fast', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    get.mockImplementation(async () => structuredClone(baseline))
    save.mockImplementation(async (group_id, revision, config) => ({ ...structuredClone(baseline), group_id, revision: revision + 1, config }))
  })
  afterEach(() => { vi.useRealTimers() })

  it('saves group rules, quota usage thresholds and final window with revision', async () => {
    const w = await open()
    expect(get).toHaveBeenCalledWith(7)
    expect(w.get('[data-testid="auto-fast-group"]').text()).not.toContain('Claude')
    await w.get('[data-testid="auto-fast-enabled"]').setValue(true)
    for (const [field, value] of Object.entries({ 'minutes-0': 60, 'used-0': 40, 'minutes-1': 25, 'used-1': 70, 'final': 3, 'age': 600 })) await w.get(`[data-testid="auto-fast-${field}"]`).setValue(value)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save).toHaveBeenCalledWith(7, 3, { enabled: true, require_multiple_accounts: false, stages: [{ minutes: 60, used_below_percent: 40 }, { minutes: 25, used_below_percent: 70 }, { minutes: 15, used_below_percent: 80 }], final_minutes: 3, usage_max_age_seconds: 600 })
    expect(w.emitted('changed')).toHaveLength(1); expect(w.get('[role="status"]').text()).toContain('saved'); w.unmount()
  })

  it.each([['minutes-0', 0], ['minutes-0', 10081], ['minutes-1', 45], ['minutes-1', 46], ['minutes-0', 1.5], ['used-0', -1], ['used-0', 101], ['used-0', ''], ['final', 0], ['final', 15], ['age', 29], ['age', 86401]])('rejects invalid %s=%s', async (field, value) => {
    const w = await open(); await w.get(`[data-testid="auto-fast-${field}"]`).setValue(value)
    await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled(); w.unmount()
  })

  it('saves the multiple-account prerequisite and defaults legacy configurations to off', async () => {
    const legacy = structuredClone(baseline)
    Reflect.deleteProperty(legacy.config, 'require_multiple_accounts')
    get.mockResolvedValue(legacy)
    const w = await open()
    expect((w.get('[data-testid="auto-fast-require-multiple"]').element as HTMLInputElement).checked).toBe(false)
    await w.get('[data-testid="auto-fast-require-multiple"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save).toHaveBeenCalledWith(7, 3, expect.objectContaining({ require_multiple_accounts: true }))
    await w.get('[data-testid="auto-fast-group"]').setValue(8); await flushPromises()
    expect((w.get('[data-testid="auto-fast-require-multiple"]').element as HTMLInputElement).checked).toBe(false)
    w.unmount()
  })

  it.each([1, 2])('shows the prerequisite status for %i available accounts', async count => {
    get.mockResolvedValue({ ...structuredClone(baseline), config: { ...baseline.config, enabled: true, require_multiple_accounts: true }, checked_at: new Date().toISOString(), eligible_accounts: count })
    const w = await open()
    expect(w.get('[data-testid="auto-fast-eligible-count"]').text()).toContain(count < 2 ? 'waitingForAccounts' : 'enoughAccounts')
    w.unmount()
  })

  it('keeps an unsaved prerequisite switch during a status refresh', async () => {
    const w = await open()
    await w.get('[data-testid="auto-fast-require-multiple"]').setValue(true)
    await w.get('[data-testid="auto-fast-refresh"]').trigger('click'); await flushPromises()
    expect((w.get('[data-testid="auto-fast-require-multiple"]').element as HTMLInputElement).checked).toBe(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.[2].require_multiple_accounts).toBe(true)
    w.unmount()
  })

  it('adds and removes stages, retaining at least one', async () => {
    const w = await open()
    await w.get('[data-testid="auto-fast-add"]').trigger('click')
    expect(w.find('[data-testid="auto-fast-minutes-3"]').exists()).toBe(true)
    for (const index of [3, 2, 1]) await w.get(`[data-testid="auto-fast-remove-${index}"]`).trigger('click')
    expect(w.get('[data-testid="auto-fast-remove-0"]').attributes('disabled')).toBeDefined()
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.[2].stages).toEqual([{ minutes: 45, used_below_percent: 50 }]); w.unmount()
  })

  it('preserves unsaved stage edits during status refresh and blocks external revision overwrite', async () => {
    vi.useFakeTimers()
    const w = await open()
    await w.get('[data-testid="auto-fast-used-0"]').setValue(42)
    get.mockResolvedValue({ ...structuredClone(baseline), revision: 4, config: { ...baseline.config, enabled: true }, total: 1, accounts: [{ account_id: 1, name: 'fixture', source: 'supplier_api', used_percent: 40, stage_minutes: 45, mode: 'threshold_met', message: 'rule active', fast: true, managed: true }] })
    await vi.advanceTimersByTimeAsync(10000); await flushPromises()
    expect(w.text()).toContain('Fast ON'); expect(w.text()).toContain('accountAutoFast.conflict')
    expect((w.get('[data-testid="auto-fast-used-0"]').element as HTMLInputElement).value).toBe('42')
    expect((w.get('[data-testid="auto-fast-enabled"]').element as HTMLInputElement).checked).toBe(false)
    await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled()
    await w.get('[data-testid="auto-fast-reload"]').trigger('click'); await flushPromises()
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.[1]).toBe(4); w.unmount()
  })

  it('ignores an older group response and saves the currently selected group', async () => {
    const w = await open()
    let resolveOld!: (rule: AccountAutoFastRule) => void
    get.mockImplementationOnce(() => new Promise<AccountAutoFastRule>(resolve => { resolveOld = resolve }))
    await w.get('[data-testid="auto-fast-group"]').setValue(8)
    get.mockResolvedValueOnce({ ...structuredClone(baseline), revision: 9 })
    await w.get('[data-testid="auto-fast-group"]').setValue(7); await flushPromises()
    resolveOld({ ...structuredClone(baseline), group_id: 8, revision: 20 }); await flushPromises()
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]?.slice(0, 2)).toEqual([7, 9]); w.unmount()
  })

  it('discards a refresh started before saving', async () => {
    const w = await open()
    let resolveOld!: (rule: AccountAutoFastRule) => void
    get.mockImplementationOnce(() => new Promise<AccountAutoFastRule>(resolve => { resolveOld = resolve }))
    await w.get('[data-testid="auto-fast-refresh"]').trigger('click')
    await w.get('[data-testid="auto-fast-enabled"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    resolveOld(structuredClone(baseline)); await flushPromises()
    expect((w.get('[data-testid="auto-fast-enabled"]').element as HTMLInputElement).checked).toBe(true)
    expect(w.text()).not.toContain('accountAutoFast.conflict'); w.unmount()
  })

  it('does not show a save form on load failure', async () => {
    get.mockRejectedValue(new Error('offline')); const w = await open()
    expect(w.find('form').exists()).toBe(false); expect(w.find('[role="alert"]').exists()).toBe(true); w.unmount()
  })

  it('keeps the draft and reports a save failure without claiming success', async () => {
    const w = await open(); save.mockRejectedValue(new Error('conflict'))
    await w.get('[data-testid="auto-fast-used-0"]').setValue(44)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(w.find('[role="alert"]').exists()).toBe(true); expect(w.emitted('changed')).toBeUndefined()
    expect((w.get('[data-testid="auto-fast-used-0"]').element as HTMLInputElement).value).toBe('44'); w.unmount()
  })
})
