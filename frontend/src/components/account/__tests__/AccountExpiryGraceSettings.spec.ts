import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountExpiryGraceSettings from '../AccountExpiryGraceSettings.vue'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountExpiryGrace', () => ({ accountExpiryGraceAPI: { get, save } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('account expiry grace controls', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    get.mockResolvedValue({ minutes: 60, effective_expires_at: '2026-11-01T02:00:00Z' })
    save.mockImplementation(async (_id, minutes) => ({ minutes, effective_expires_at: '2026-11-01T03:00:00Z' }))
  })
  it('loads on demand and saves custom or zero retention through its own endpoint', async () => {
    const w = mount(AccountExpiryGraceSettings, { props: { accountId: 34, active: true } })
    expect(get).not.toHaveBeenCalled()
    await w.get('[data-testid="expiry-grace-open"]').trigger('click'); await flushPromises()
    expect(get).toHaveBeenCalledWith(34)
    await w.get('[data-testid="expiry-grace-minutes"]').setValue(125)
    await w.get('[data-testid="expiry-grace-save"]').trigger('click'); await flushPromises()
    expect(save).toHaveBeenLastCalledWith(34, 125)
    await w.get('[data-testid="expiry-grace-minutes"]').setValue(0)
    await w.get('[data-testid="expiry-grace-save"]').trigger('click'); await flushPromises()
    expect(save).toHaveBeenLastCalledWith(34, 0)
    await w.get('[data-testid="expiry-grace-minutes"]').setValue(1441)
    expect(w.get('[data-testid="expiry-grace-save"]').attributes('disabled')).toBeDefined()
    w.unmount()
  })
  it('ignores a response for the previously selected account', async () => {
    let resolve!: (value: { minutes: number }) => void
    get.mockImplementationOnce(() => new Promise(r => { resolve = r }))
    const w = mount(AccountExpiryGraceSettings, { props: { accountId: 34, active: true } })
    await w.get('[data-testid="expiry-grace-open"]').trigger('click')
    await w.setProps({ accountId: 35 })
    resolve({ minutes: 120 }); await flushPromises()
    expect(w.find('[data-testid="expiry-grace-minutes"]').exists()).toBe(false)
    await w.get('[data-testid="expiry-grace-open"]').trigger('click'); await flushPromises()
    expect(get).toHaveBeenLastCalledWith(35)
    expect((w.get('[data-testid="expiry-grace-minutes"]').element as HTMLInputElement).value).toBe('60')
    w.unmount()
  })
})
