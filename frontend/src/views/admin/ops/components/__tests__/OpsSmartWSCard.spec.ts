import { describe, it, expect, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSmartWSCard from '../OpsSmartWSCard.vue'

const { getMetrics } = vi.hoisted(() => ({ getMetrics: vi.fn() }))
vi.mock('@/api/admin/ops', () => ({ getSmartWSMetrics: getMetrics }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const latency = { samples: 0, ttft_samples: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, duration_p95_ms: 0 }
const sample = { connections: 3, active: 1, acquiring: 0, handshaking: 1, limits: { connections: 64, acquiring: 32, handshakes: 4 }, counters: { succeeded: 9, dispatch_uncertain: 2 }, cold: latency, reused: { ...latency, samples: 5, ttft_samples: 4, ttft_p50_ms: 123 } }

describe('SmartWS metrics', () => {
  it('shows unknown latency as a dash and follows dashboard refresh', async () => {
    getMetrics.mockReset().mockResolvedValue(sample)
    const wrapper = mount(OpsSmartWSCard, { props: { refreshToken: 0 } })
    await flushPromises()
    expect(wrapper.text()).toContain('3 / 64')
    expect(wrapper.text()).toContain('123 ms')
    expect(wrapper.text()).toContain('—')
    expect(wrapper.text()).toContain('admin.ops.smartWS.dispatch_uncertain')
    const signal = getMetrics.mock.calls[0][0].signal as AbortSignal
    await wrapper.setProps({ refreshToken: 1 })
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(getMetrics).toHaveBeenCalledTimes(2)
    const finalSignal = getMetrics.mock.calls[1][0].signal as AbortSignal
    wrapper.unmount()
    expect(finalSignal.aborted).toBe(true)
  })

  it('shows failed retrieval without inventing a healthy zero state', async () => {
    getMetrics.mockReset().mockRejectedValue(new Error('unavailable'))
    const wrapper = mount(OpsSmartWSCard, { props: { refreshToken: 0 } })
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('loadFailed')
    expect(wrapper.find('table').exists()).toBe(false)
    wrapper.unmount()
  })
})
