vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isReadOnlyAdmin: false }) }))

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import ProxiesView from '../ProxiesView.vue'

const { list, get, save } = vi.hoisted(() => ({ list: vi.fn(), get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { list, getAllWithCount: vi.fn().mockResolvedValue([]) } } }))
vi.mock('@/api/admin/proxyBindingLimits', () => ({ proxyBindingLimitsAPI: { get, save } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
const mountView = () => shallowMount(ProxiesView, { global: { stubs: {
  AppLayout: { template: '<div><slot /></div>' },
  TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /></div>' },
  DataTable: { props: ['data'], template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-select" :row="row" /><slot name="cell-account_count" :row="row" :value="row.account_count" /></div></div>' },
  BaseDialog: { props: ['show', 'title'], template: '<div v-if="show" :data-title="title"><slot /><slot name="footer" /></div>' }
} } })
beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue({ items: [{ id: 1, name: 'one', account_count: 2 }, { id: 2, name: 'two', account_count: 0 }], total: 2, pages: 1 })
  get.mockResolvedValue([{ proxy_id: 1, max_accounts: 3, account_count: 2 }, { proxy_id: 2, max_accounts: 1, account_count: 0 }])
  save.mockResolvedValue([])
})
describe('proxy binding capacity controls', () => {
  it('loads saved limits and changes a single proxy', async () => {
    const w = mountView(); await flushPromises()
    expect(w.text()).toContain('accountRefill.bindingLimit: 3')
    await w.findAll('button').find(b => b.text() === 'accountRefill.bindingLimit: 3')!.trigger('click')
    const dialog = w.get('[data-title="accountRefill.bindingLimitTitle"]')
    expect((dialog.get('input').element as HTMLInputElement).value).toBe('3')
    await dialog.get('input').setValue(1)
    await dialog.get('button').trigger('click'); await flushPromises()
    expect(save).toHaveBeenCalledWith([1], 1)
    w.unmount()
  })
  it('sets the selected proxies in one request', async () => {
    const w = mountView(); await flushPromises()
    for (const checkbox of w.findAll('input[type="checkbox"]')) await checkbox.setValue(true)
    await w.findAll('button').find(b => b.text() === 'accountRefill.batchBindingLimits')!.trigger('click')
    const dialog = w.get('[data-title="accountRefill.bindingLimitTitle"]')
    await dialog.get('input').setValue(4)
    await dialog.get('button').trigger('click'); await flushPromises()
    expect(save).toHaveBeenCalledWith([1, 2], 4)
    w.unmount()
  })
})
