import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import GroupFirstTokenModal from '../GroupFirstTokenModal.vue'
import type { AdminGroup } from '@/types'
const m = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), update: vi.fn(), showSuccess: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: m }))
vi.mock('@/api/admin', () => ({ adminAPI: { groups: { update: m.update } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => m }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
beforeEach(() => { vi.resetAllMocks(); m.get.mockResolvedValue({ data: { early_rate_limits_enabled: false } }); m.put.mockResolvedValue({}); m.update.mockResolvedValue({}) })
function open() {
 return mount(GroupFirstTokenModal, { props: { show: true, group: { id: 7, name: 'Main', smart_ws_mode: 'enabled' } as AdminGroup }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' } } } })
}
describe('group first token controls', () => {
 it('saves early limits independently and reloads the saved value', async () => {
  let enabled = false
  m.get.mockImplementation(() => Promise.resolve({ data: { early_rate_limits_enabled: enabled } }))
  m.put.mockImplementation((_path: string, value: {early_rate_limits_enabled: boolean}) => { enabled=value.early_rate_limits_enabled; return Promise.resolve({}) })
  const w=open(); await flushPromises()
  await w.get('[data-testid="group-early-rate-limits"]').setValue(true)
  await w.get('.btn-primary').trigger('click'); await flushPromises()
  expect(m.put).toHaveBeenCalledWith('/admin/settings/group-first-token-optimization/7', { early_rate_limits_enabled: true })
  expect(m.update).not.toHaveBeenCalled(); expect(w.emitted('close')).toHaveLength(1)
  await w.setProps({show:false}); await w.setProps({show:true}); await flushPromises()
  expect((w.get('[data-testid="group-early-rate-limits"]').element as HTMLInputElement).checked).toBe(true)
 })
 it('prevents duplicate saves and keeps failures open', async () => {
  m.put.mockRejectedValue(new Error('offline'))
  const w=open(); await flushPromises()
  await w.get('.btn-primary').trigger('click'); await flushPromises()
  expect(m.showError).toHaveBeenCalled(); expect(w.emitted('close')).toBeUndefined()
 })
 it('prevents saving when settings cannot load', async () => {
  m.get.mockRejectedValue(new Error('offline'))
  const w=open(); await flushPromises()
  expect(w.get('.btn-primary').attributes('disabled')).toBeDefined()
  expect(m.put).not.toHaveBeenCalled()
 })
})
