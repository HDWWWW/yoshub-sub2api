import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRetryDefaults from '../AccountRetryDefaults.vue'
const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountRetry', () => ({ accountRetryAPI: { get, save } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
async function open() {
 const w = mount(AccountRetryDefaults, { global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } } })
 await w.get('[data-testid="open-retry-default"]').trigger('click'); await flushPromises(); return w
}
describe('default account retries', () => {
 beforeEach(() => { vi.resetAllMocks(); get.mockResolvedValue({ count: 3 }); save.mockResolvedValue({ count: 0 }) })
 it.each([0, 7, 10])('loads and persists %s', async n => {
  const w = await open(); const input = w.get<HTMLInputElement>('[data-testid="retry-default-count"]')
  expect(input.element.value).toBe('3'); await input.setValue(n)
  await w.get('form').trigger('submit'); await flushPromises(); expect(save).toHaveBeenCalledWith(n); w.unmount()
 })
 it('does not overwrite the default after a failed load', async () => {
  get.mockRejectedValue(new Error('offline')); const w = await open()
  await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled(); expect(w.get('[role="alert"]').exists()).toBe(true); w.unmount()
 })
 it.each([-1, 11, 1.5])('rejects %s', async n => {
  const w = await open(); await w.get('[data-testid="retry-default-count"]').setValue(n)
  await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled(); w.unmount()
 })
})
