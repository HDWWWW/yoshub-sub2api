import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import HappyFirstTokenView from '../HappyFirstTokenView.vue'
import happyFirstToken from '@/i18n/locales/zh/happyFirstToken'

const { getSettings, updateSettings, showSuccess, showError } = vi.hoisted(() => ({
  getSettings: vi.fn(), updateSettings: vi.fn(), showSuccess: vi.fn(), showError: vi.fn()
}))
vi.mock('@/api/admin/settings', () => ({
  getHappyFirstTokenSettings: getSettings,
  updateHappyFirstTokenSettings: updateSettings
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({
  t: (key: string, params: Record<string, unknown> = {}) => {
    const message = happyFirstToken[key.replace('happyFirstToken.', '') as keyof typeof happyFirstToken]
    return (message || key).replace(/\{(\w+)\}/g, (_, token) => String(params[token] ?? ''))
  }
}) }))

async function openPage() {
  const wrapper = mount(HappyFirstTokenView, {
    global: {
      stubs: { AppLayout: { template: '<div><slot /></div>' } }
    }
  })
  await flushPromises()
  return wrapper
}

describe('HappyFirstTokenView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getSettings.mockResolvedValue({ enabled: false, min_delay_ms: 100, max_delay_ms: 1000 })
    updateSettings.mockImplementation(async (settings) => settings)
  })

  it('loads seconds, saves milliseconds and restores persisted settings on reload', async () => {
    const wrapper = await openPage()
    expect((wrapper.get('#happy-minimum').element as HTMLInputElement).value).toBe('0.1')
    expect(wrapper.get('[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('#happy-minimum').setValue('0.125')
    await wrapper.get('#happy-maximum').setValue('0.9')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ enabled: true, min_delay_ms: 125, max_delay_ms: 900 })
    expect(showSuccess).toHaveBeenCalledOnce()
    expect(wrapper.get('[type="submit"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
    getSettings.mockResolvedValue(updateSettings.mock.calls[0]![0])
    const reloaded = await openPage()
    expect(reloaded.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect((reloaded.get('#happy-minimum').element as HTMLInputElement).value).toBe('0.125')
    reloaded.unmount()
  })

  it.each([['1.1', '1'], ['-1', '1'], ['0', '61'], ['', '1']])('rejects invalid bounds %s–%s', async (min, max) => {
    const wrapper = await openPage()
    await wrapper.get('#happy-minimum').setValue(min)
    await wrapper.get('#happy-maximum').setValue(max)
    await wrapper.get('form').trigger('submit')
    expect(updateSettings).not.toHaveBeenCalled()
    expect(wrapper.get('[type="submit"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[role="alert"]').text()).toContain('有效时间')
    wrapper.unmount()
  })

  it('accepts equal bounds as a fixed delay and zero as immediate', async () => {
    const wrapper = await openPage()
    await wrapper.get('#happy-minimum').setValue('0')
    await wrapper.get('#happy-maximum').setValue('0')
    expect(wrapper.text()).toContain('固定等待 0 秒')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ enabled: false, min_delay_ms: 0, max_delay_ms: 0 })
    wrapper.unmount()
  })

  it('keeps failed edits available to retry and never reports them as saved', async () => {
    updateSettings.mockRejectedValue(new Error('offline'))
    const wrapper = await openPage()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(showError).toHaveBeenCalledOnce()
    expect(showSuccess).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('有未保存的更改')
    expect(wrapper.get('[type="submit"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('does not let a failed load overwrite existing settings with defaults', async () => {
    getSettings.mockRejectedValueOnce(new Error('offline'))
    const wrapper = await openPage()
    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('读取设置失败')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.find('form').exists()).toBe(true)
    expect(updateSettings).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
