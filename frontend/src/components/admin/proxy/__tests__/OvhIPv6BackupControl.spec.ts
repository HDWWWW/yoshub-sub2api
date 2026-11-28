import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OvhIPv6BackupControl from '../OvhIPv6BackupControl.vue'
const { getBackup, setBackup, retirements } = vi.hoisted(() => ({ getBackup: vi.fn(), setBackup: vi.fn(), retirements: vi.fn() }))
vi.mock('@/api/admin/ovhIPv6', () => ({ ovhIPv6API: { getBackup, setBackup, retirements } }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (k: string) => k }) }))
const base = { enabled: false, available: true, configured: true, prefix: '2001:db8:1::/64', bound: 0 }
beforeEach(() => { vi.resetAllMocks(); getBackup.mockResolvedValue(base) })
describe('standby pool', () => {
  it('starts in manual mode and displays only confirmed changes', async () => {
    const w = mount(OvhIPv6BackupControl)
    await flushPromises()
    const control = w.get('[role="switch"]')
    expect(control.attributes('aria-checked')).toBe('false')
    setBackup.mockRejectedValueOnce(new Error('offline'))
    await control.trigger('click'); await flushPromises()
    expect(control.attributes('aria-checked')).toBe('false')
    setBackup.mockResolvedValueOnce({ ...base, enabled: true })
    await control.trigger('click'); await flushPromises()
    expect(control.attributes('aria-checked')).toBe('true')
    expect(setBackup).toHaveBeenLastCalledWith(true)
  })
  it('loads retirement history on demand with the address and pool', async () => {
    const w = mount(OvhIPv6BackupControl)
    await flushPromises()
    expect(retirements).not.toHaveBeenCalled()
    retirements.mockResolvedValueOnce([{ id: 2, address: '2001:db8:1::2', pool: 'backup', retired_at: '2026-11-27T18:00:00Z' }])
    await w.get('button[aria-expanded]').trigger('click'); await flushPromises()
    expect(retirements).toHaveBeenCalledWith(0)
    expect(w.get('tbody').text()).toContain('2001:db8:1::2')
    expect(w.get('tbody').text()).toContain('ovhIPv6.backup')
  })
})