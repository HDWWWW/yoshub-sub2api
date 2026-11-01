import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import BulkEditAccountModal from '../BulkEditAccountModal.vue'

const { post, get } = vi.hoisted(() => ({ post: vi.fn(), get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post, get } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.clearAllMocks()
  get.mockResolvedValue({ data: [] })
  post.mockResolvedValue({ data: { success: 2, failed: 0, results: [] } })
})

function open(filtered: boolean) {
  return mount(BulkEditAccountModal, {
    props: {
      show: true, accountIds: filtered ? [] : [7, 8], selectedPlatforms: ['openai'], selectedTypes: ['oauth'], proxies: [], groups: [],
      ...(filtered ? { target: { mode: 'filtered' as const, previewCount: 21, filters: { platform: 'openai', group: '4', search: 'renewal' }, selectedPlatforms: ['openai'], selectedTypes: ['oauth'] } } : {})
    },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, ConfirmDialog: true, Select: true, GroupSelector: true, Icon: true, ModelWhitelistSelector: true } }
  })
}

describe('bulk OVH IPv6 selection through the actual API wrapper', () => {
  it.each([false, true])('preserves a real OVH option click in the HTTP request (filtered=%s)', async (filtered) => {
    const wrapper = open(filtered)
    await wrapper.get('#bulk-edit-proxy-enabled').setValue(true)
    await wrapper.get('#bulk-edit-proxy-body .select-trigger').trigger('click')
    await wrapper.get('[data-proxy-mode="ovh_ipv6"]').trigger('click')
    expect(wrapper.get('#bulk-edit-proxy-body .select-trigger').text()).toContain('ovhIPv6.mode.ovh_ipv6')
    await wrapper.get('#bulk-edit-account-form').trigger('submit')
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/admin/accounts/bulk-update', {
      ...(filtered ? { filters: { platform: 'openai', group: '4', search: 'renewal' } } : { account_ids: [7, 8] }),
      proxy_mode: 'ovh_ipv6', proxy_id: null
    }, { timeout: 300000 })
  })

  it('replaces the automatic choice when the user explicitly selects no proxy', async () => {
    const wrapper = open(false)
    await wrapper.get('#bulk-edit-proxy-enabled').setValue(true)
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('[data-proxy-mode="ovh_ipv6"]').trigger('click')
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.findAll('.select-option').find(option => option.text() === 'admin.accounts.noProxy')!.trigger('click')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(post).toHaveBeenCalledWith('/admin/accounts/bulk-update', { account_ids: [7, 8], proxy_mode: 'direct', proxy_id: 0 })
  })

  it('shows actual applied addresses and failed account reasons without allowing a second submission', async () => {
    post.mockResolvedValue({ data: { success: 1, failed: 1, results: [
      { account_id: 7, success: true, proxy_id: 10, managed_ipv6: '2001:db8::10' },
      { account_id: 8, success: false, error: 'allocation failed' }
    ] } })
    const wrapper = open(false)
    await wrapper.get('#bulk-edit-proxy-enabled').setValue(true)
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('[data-proxy-mode="ovh_ipv6"]').trigger('click')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[data-testid="bulk-proxy-result"]').text()).toContain('2001:db8::10')
    expect(wrapper.get('[data-testid="bulk-proxy-result"]').text()).toContain('#8')
    expect(wrapper.get('[data-testid="bulk-proxy-result"]').text()).toContain('allocation failed')
    expect(wrapper.find('button[type="submit"]').exists()).toBe(false)
    expect(wrapper.emitted('updated')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    await wrapper.findAll('button').find(button => button.text() === 'admin.accounts.bulkEdit.closeAndRefresh')!.trigger('click')
    expect(wrapper.emitted('updated')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
    await wrapper.setProps({ show: false }); await wrapper.setProps({ show: true })
    expect(wrapper.find('[data-testid="bulk-proxy-result"]').exists()).toBe(false)
    expect(wrapper.find('button[type="submit"]').exists()).toBe(true)
    expect(post).toHaveBeenCalledTimes(1)
  })

  it('treats an interrupted automatic batch as unconfirmed and requires a list refresh before another operation', async () => {
    post.mockRejectedValue({ status: 0, code: 'ECONNABORTED' })
    const wrapper = open(false)
    await wrapper.get('#bulk-edit-proxy-enabled').setValue(true)
    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('[data-proxy-mode="ovh_ipv6"]').trigger('click')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('admin.accounts.bulkEdit.proxyResultUnconfirmed')
    expect(wrapper.find('button[type="submit"]').exists()).toBe(false)
    expect(post).toHaveBeenCalledTimes(1)
  })
})
