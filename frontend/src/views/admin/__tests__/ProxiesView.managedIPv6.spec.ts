import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import ProxiesView from '../ProxiesView.vue'

const { list, getAllWithCount, get, save, batchDelete } = vi.hoisted(() => ({
  list: vi.fn(), getAllWithCount: vi.fn(), get: vi.fn(), save: vi.fn(), batchDelete: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { list, getAllWithCount, batchDelete } } }))
vi.mock('@/api/admin/proxyBindingLimits', () => ({ proxyBindingLimitsAPI: { get, save } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))

enableAutoUnmount(afterEach)

const managed = {
  id: 1, name: 'assigned-ipv6', managed_ipv6: '2001:db8:1234::10',
  host: 'egress.internal', port: 1080, protocol: 'socks5',
  username: 'internal-user', password: 'internal-secret', account_count: 1
}
const manual = { id: 2, name: 'imported-proxy', host: 'proxy.example', port: 8080, protocol: 'http', account_count: 0 }

const mountView = () => shallowMount(ProxiesView, { global: { stubs: {
  AppLayout: { template: '<div><slot /></div>' },
  TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
  DataTable: { props: ['data'], template: `<div><div v-for="row in data" :key="row.id" :data-row="row.id">
    <slot name="cell-select" :row="row" /><slot name="cell-name" :row="row" :value="row.name" />
    <slot name="cell-address" :row="row" /><slot name="cell-auth" :row="row" />
    <slot name="cell-account_count" :row="row" :value="row.account_count" /><slot name="cell-actions" :row="row" />
  </div></div>` },
  Pagination: { emits: ['update:page'], template: '<button data-test="next-page" @click="$emit(\'update:page\', 2)">next</button>' },
  BaseDialog: { props: ['show', 'title'], template: '<div v-if="show" :data-title="title"><slot /><slot name="footer" /></div>' },
  ConfirmDialog: { props: ['show'], emits: ['confirm'], template: '<button v-if="show" data-test="confirm-delete" @click="$emit(\'confirm\')">confirm</button>' }
} } })

beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue({ items: [managed, manual], total: 2, pages: 1 })
  getAllWithCount.mockResolvedValue([managed, manual])
  get.mockResolvedValue([{ proxy_id: 2, max_accounts: 3, account_count: 0 }])
  save.mockResolvedValue([])
  batchDelete.mockResolvedValue({ deleted_ids: [2], skipped: [] })
})

describe('managed IPv6 proxy entries', () => {
  it('shows the egress address and fixed exclusive binding without credentials or mutable actions', async () => {
    const wrapper = mountView()
    await flushPromises()
    const row = wrapper.get('[data-row="1"]')
    expect(row.text()).toContain('2001:db8:1234::10')
    expect(row.text()).toContain('ovhIPv6.source')
    expect(row.text()).toContain('ovhIPv6.fixedBindingLimit')
    for (const hidden of ['egress.internal', 'internal-user', 'internal-secret', 'common.edit', 'common.delete']) {
      expect(row.text()).not.toContain(hidden)
    }
    expect(row.find('[title="admin.proxies.copyProxyUrl"]').exists()).toBe(false)
    expect(row.text()).toContain('admin.proxies.testConnection')
    expect(wrapper.get('[data-row="2"]').text()).toContain('common.edit')
    expect(wrapper.get('[data-row="2"]').text()).toContain('common.delete')
    expect(get).toHaveBeenCalledWith([2], expect.any(AbortSignal))
  })

  it('excludes managed selections from bulk binding changes across pages', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-row="1"] input[type="checkbox"]').setValue(true)
    list.mockResolvedValue({ items: [manual], total: 2, pages: 2 })
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-row="2"] input[type="checkbox"]').setValue(true)
    await wrapper.findAll('button').find(button => button.text() === 'accountRefill.batchBindingLimits')!.trigger('click')
    const dialog = wrapper.get('[data-title="accountRefill.bindingLimitTitle"]')
    await dialog.get('input').setValue(4)
    await dialog.get('button').trigger('click')
    await flushPromises()
    expect(save).toHaveBeenCalledWith([2], 4)
  })

  it('excludes managed selections from bulk deletion', async () => {
    const wrapper = mountView()
    await flushPromises()
    for (const checkbox of wrapper.findAll('input[type="checkbox"]')) await checkbox.setValue(true)
    await wrapper.get('button[title="admin.proxies.batchDeleteAction"]').trigger('click')
    await wrapper.get('[data-test="confirm-delete"]').trigger('click')
    await flushPromises()
    expect(batchDelete).toHaveBeenCalledWith([2])
  })
})
