import { describe, it, vi, beforeEach } from 'vitest'
import { expect } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountSchedulingModal from '../AccountSchedulingModal.vue'

const { preview, update } = vi.hoisted(() => ({ preview: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin/accountScheduling', () => ({ accountSchedulingAPI: { preview } }))
vi.mock('@/api/admin/accounts', () => ({ update }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => value }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

beforeEach(() => {
  vi.resetAllMocks()
  preview.mockResolvedValue({ expiry_priority_enabled: true, items: [{ account_id: 38, name: 'account', automatic_priority: 3, effective_priority: 0, manual_priority: 0, effective_expires_at: null }] })
  update.mockResolvedValue({ id: 38 })
})
async function open() {
  const w = mount(AccountSchedulingModal, { props: { show: false, groups: [] }, global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } } })
  await w.setProps({ show: true }); await flushPromises()
  return w
}
describe('internal scheduling controls', () => {
  it('shows automatic and effective values, saves zero override, and restores without changing native priority', async () => {
    const w = await open()
    expect(preview).toHaveBeenCalledWith(0, 'openai')
    expect(w.findAll('td').map(td => td.text())).toEqual(expect.arrayContaining(['3', '0accountRefill.priorityManual']))
    await w.findAll('button').find(b => b.text() === 'common.save')!.trigger('click'); await flushPromises()
    expect(update).toHaveBeenLastCalledWith(38, { yoshub_priority_mode: 'manual', priority: 0 })
    await w.findAll('button').find(b => b.text() === 'accountRefill.restoreAutomatic')!.trigger('click'); await flushPromises()
    expect(update).toHaveBeenLastCalledWith(38, { yoshub_priority_mode: 'auto' })
    expect(w.emitted('updated')).toHaveLength(2)
    w.unmount()
  })
  it('keeps values and displays errors on failed save', async () => {
    const w = await open()
    update.mockRejectedValueOnce(new Error('offline'))
    await w.get('input').setValue(10000)
    expect(w.findAll('button').find(b => b.text() === 'common.save')!.attributes('disabled')).toBeDefined()
    await w.get('input').setValue(2)
    await w.findAll('button').find(b => b.text() === 'common.save')!.trigger('click'); await flushPromises()
    expect(w.find('[role="alert"]').exists()).toBe(true)
    expect(w.emitted('updated')).toBeUndefined()
    w.unmount()
  })
})
