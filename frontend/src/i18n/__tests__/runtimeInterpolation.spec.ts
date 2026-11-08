import { mount } from '@vue/test-utils'
import { beforeAll, describe, expect, it } from 'vitest'
import AutoRefreshButton from '@/components/common/AutoRefreshButton.vue'
import UsageProgressBar from '@/components/account/UsageProgressBar.vue'
import { i18n, loadLocaleMessages } from '..'

// Exercise the actual lazy-loaded messages and the same i18n build as Vite.
// A key-only t() mock cannot catch missing runtime message compilation.
describe('runtime message interpolation', () => {
  beforeAll(async () => {
    await loadLocaleMessages('zh')
    await loadLocaleMessages('en')
    i18n.global.locale.value = 'zh'
  })

  it.each([
    ['accountRefill.rotationPosition', { position: 2 }, '周期内第 2 个'],
    ['accountRefill.observationProgress', { elapsed: 0, required: 30, utilization: 0 }, '已持续 0 / 30 秒 · 当前利用率 0%'],
    ['accountRefill.observationProgress', { elapsed: 15, required: 30, utilization: 82.5 }, '已持续 15 / 30 秒 · 当前利用率 82.5%'],
    ['accountRefill.observationNoCapacity', { elapsed: 10, required: 30 }, '已持续 10 / 30 秒 · 有运行中请求，但没有剩余容量'],
    ['accountRefill.forecastCapacity', { count: 2, current: 120, projected: 60, pending: 0, inUse: 42 }, '预计退出 2 个 · 当前容量 120 → 退出后容量 60 · 在途抵扣容量 0 · 保留运行中并发 42'],
    ['accountRefill.forecastProgress', { elapsed: 20, required: 30, utilization: 70 }, '已持续 20 / 30 秒 · 抵扣在途后预计利用率 70%'],
    ['common.autoRefresh.seconds', { n: 15 }, '15 秒'],
    ['admin.accounts.autoRefreshCountdown', { seconds: 10 }, '自动刷新：10 秒'],
  ])('renders %s with real values', (key, values, expected) => {
    expect(i18n.global.t(key, values)).toBe(expected)
  })

  it('keeps interpolation working when switching locales', () => {
    i18n.global.locale.value = 'en'
    try {
      expect(i18n.global.t('accountRefill.rotationPosition', { position: 2 })).toBe('Order 2 in cycle')
    } finally {
      i18n.global.locale.value = 'zh'
    }
    expect(i18n.global.t('accountRefill.rotationPosition', { position: 3 })).toBe('周期内第 3 个')
  })

  it.each([
    [123.456, '预计总费用 $123.46'],
    [0, '预计总费用 $0.00'],
  ])('renders estimated total cost %s in Chinese with an actual amount', (cost, expected) => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '7d', utilization: 77, color: 'emerald', estimatedTotalCost: cost,
        windowStats: { requests: 707, tokens: 74000000, cost: 131.49, user_cost: 19.94 }
      },
      global: { plugins: [i18n] }
    })
    try {
      expect(wrapper.get('[data-test="estimated-total-cost"]').text()).toBe(expected)
    } finally {
      wrapper.unmount()
    }
  })

  it('updates the Chinese countdown including zero and the interval options', async () => {
    const wrapper = mount(AutoRefreshButton, {
      props: { enabled: true, intervalSeconds: 15, countdown: 10, intervals: [15, 30] },
      global: { plugins: [i18n] }
    })
    try {
      expect(wrapper.text()).toBe('自动刷新：10 秒')
      await wrapper.setProps({ countdown: 0 })
      expect(wrapper.text()).toBe('自动刷新：0 秒')
      await wrapper.get('button').trigger('click')
      expect(wrapper.text()).toContain('15 秒')
      expect(wrapper.text()).toContain('30 秒')
      expect(wrapper.text()).not.toMatch(/\{\w+\}/)
    } finally {
      wrapper.unmount()
    }
  })
})
