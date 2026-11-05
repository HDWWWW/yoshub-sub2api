import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { createPinia } from 'pinia'
import { apiClient } from '@/api/client'
import ExcelBPSSettings from '../ExcelBPSSettings.vue'
import zh from '@/i18n/locales/zh/admin/excelBps'

vi.mock('@/api/client', () => ({ apiClient: { get: vi.fn(), put: vi.fn() } }))

const defaults = () => ({
  enabled: true,
  images: {
    Enabled: true, Mode: 'native', BaseURL: '', BodyLimitMiB: 64, BudgetMiB: 1024,
    MaxRequests: 128, Policy: 'off', WarningRemaining: 8, CompactReserve: 3,
    Limits: { MaxImageMiB: 20, MaxImages: 64, MaxTotalMiB: 64, StorageMiB: 1024, StorageEntries: 4096, TTLMinutes: 30 }
  }
})

function create() {
  return mount(ExcelBPSSettings, { global: { plugins: [createPinia(), createI18n({
    legacy: false, locale: 'zh', messages: { zh: { admin: { settings: { excelBps: zh } },
      common: { loading: '加载中', save: '保存', saved: '已保存', saving: '保存中', refresh: '重试' }
    } }
  })] } })
}

beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(apiClient.get).mockResolvedValue({ data: defaults() })
  vi.mocked(apiClient.put).mockResolvedValue({ data: defaults() })
})

describe('Excel BPS settings', () => {
  it('preserves image settings when the global route is switched off and saves to its dedicated endpoint', async () => {
    const wrapper = create()
    await flushPromises()
    expect(apiClient.get).toHaveBeenCalledWith('/admin/settings/excel-bps')
    await wrapper.findAll('input[type="checkbox"]')[0]!.setValue(false)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(apiClient.put).toHaveBeenCalledWith('/admin/settings/excel-bps', { ...defaults(), enabled: false })
    wrapper.unmount()
  })

  it('saves relay origin and renders a server validation failure', async () => {
    vi.mocked(apiClient.put).mockRejectedValue({ response: { data: { message: 'invalid relay origin' } } })
    const wrapper = create()
    await flushPromises()
    await wrapper.findAll('select')[0]!.setValue('relay')
    await wrapper.get('input[type="url"]').setValue('https://images.example.com')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(apiClient.put).toHaveBeenCalledWith('/admin/settings/excel-bps', expect.objectContaining({
      images: expect.objectContaining({ Mode: 'relay', BaseURL: 'https://images.example.com' })
    }))
    expect(wrapper.get('[role="alert"]').text()).toContain('invalid relay origin')
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('does not show a save form when settings cannot be loaded and supports retry', async () => {
    vi.mocked(apiClient.get).mockRejectedValueOnce(new Error('offline'))
    const wrapper = create()
    await flushPromises()
    expect(wrapper.find('form').exists()).toBe(false)
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('form').exists()).toBe(true)
    wrapper.unmount()
  })
})
