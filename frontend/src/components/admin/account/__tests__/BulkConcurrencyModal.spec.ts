import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BulkConcurrencyModal from '../BulkConcurrencyModal.vue'

const { list, bulkUpdate, showSuccess } = vi.hoisted(() => ({
  list: vi.fn(),
  bulkUpdate: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { list, bulkUpdate } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess }) }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, values?: Record<string, unknown>) => `${key} ${JSON.stringify(values ?? {})}` })
}))

function mountModal(filters = { group: '12', platform: 'openai', search: 'target' }) {
  return mount(BulkConcurrencyModal, {
    props: { show: true, filters, groupLabel: 'Team 12' },
    global: {
      stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } }
    }
  })
}

describe('BulkConcurrencyModal', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    list.mockResolvedValue({ items: [], total: 501 })
    bulkUpdate.mockResolvedValue({ success: 501, failed: 0, results: [] })
  })

  it('updates every matching page using frozen group filters, without priority or selected IDs', async () => {
    const filters = { group: '12', platform: 'openai', search: 'target', status: 'active', type: 'oauth', privacy_mode: 'training_disabled' }
    const wrapper = mountModal(filters)
    await flushPromises()
    expect(list).toHaveBeenCalledWith(1, 1, filters)
    expect(wrapper.get('[data-testid="concurrency-scope"]').text()).toContain('501')
    await wrapper.setProps({ filters: { group: '15' } })
    await wrapper.get('input').setValue('37')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(bulkUpdate).toHaveBeenCalledWith({ filters, concurrency: 37 })
    expect(wrapper.emitted('updated')).toHaveLength(1)
    expect(showSuccess).toHaveBeenCalledOnce()
    wrapper.unmount()
  })

  it.each(['', '0', '-1', '1.5', '2147483648'])('blocks invalid concurrency %s', async (value) => {
    const wrapper = mountModal()
    await flushPromises()
    await wrapper.get('input').setValue(value)
    expect(wrapper.get('[data-testid="apply-bulk-concurrency"]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('requires a group and accepts the explicit ungrouped filter', async () => {
    const wrapper = mountModal({ group: '', platform: '', search: '' })
    await flushPromises()
    expect(list).not.toHaveBeenCalled()
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).not.toHaveBeenCalled()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true, filters: { group: 'ungrouped' } })
    await flushPromises()
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).toHaveBeenCalledWith({ filters: { group: 'ungrouped' }, concurrency: 50 })
    wrapper.unmount()
  })

  it('blocks updates when the range preview fails or matches no accounts', async () => {
    list.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('previewFailed')
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).not.toHaveBeenCalled()
    list.mockResolvedValueOnce({ items: [], total: 0 })
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('prevents duplicate writes while saving and leaves failed updates open', async () => {
    let reject!: (error: Error) => void
    bulkUpdate.mockImplementationOnce(() => new Promise((_, rejectPromise) => { reject = rejectPromise }))
    const wrapper = mountModal()
    await flushPromises()
    await wrapper.get('form').trigger('submit')
    await wrapper.get('form').trigger('submit')
    expect(bulkUpdate).toHaveBeenCalledOnce()
    expect(wrapper.get('input').attributes('disabled')).toBeDefined()
    reject(new Error('write failed'))
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('write failed')
    expect(wrapper.emitted('updated')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores stale previews after closing and reopening with another group', async () => {
    let resolveOld!: (value: { total: number }) => void
    list.mockImplementationOnce(() => new Promise((resolve) => { resolveOld = resolve }))
    const wrapper = mountModal()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true, filters: { group: '15' } })
    await flushPromises()
    resolveOld({ total: 999 })
    await flushPromises()
    expect(wrapper.get('[data-testid="concurrency-scope"]').text()).toContain('501')
    expect(wrapper.get('[data-testid="concurrency-scope"]').text()).not.toContain('999')
    wrapper.unmount()
  })
})
