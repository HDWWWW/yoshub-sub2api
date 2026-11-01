import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import ProxySelector from '../ProxySelector.vue'
import type { Proxy } from '@/types'

const testProxy = vi.hoisted(() => vi.fn())
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { testProxy } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
beforeEach(() => { vi.clearAllMocks() })

async function openSelector() {
  const wrapper = mount(ProxySelector, {
    props: { modelValue: null, proxies: [1, 2].map(id => ({
      id, name: `Proxy ${id}`, host: 'localhost', port: 8080, protocol: 'http'
    } as Proxy)) },
    global: { stubs: { Icon: true } }
  })
  await wrapper.get('.select-trigger').trigger('click')
  return wrapper
}

describe('proxy connection tests', () => {
  it('does not restart an individual test when a batch is started', async () => {
    let finish!: (result: object) => void
    testProxy.mockImplementation((id: number) => id === 1
      ? new Promise(resolve => { finish = resolve })
      : Promise.resolve({ success: true, country: 'GB' }))
    const wrapper = await openSelector()
    await wrapper.findAll('.test-btn')[0].trigger('click')
    await wrapper.get('.batch-test-btn').trigger('click')
    await flushPromises()
    expect(testProxy.mock.calls.map(([id]) => id)).toEqual([1, 2])
    expect(wrapper.findAll('.test-btn')[0].attributes('disabled')).toBeDefined()
    finish({ success: true, country: 'US' })
    await flushPromises()
    expect(wrapper.text()).toContain('US')
    expect(wrapper.findAll('.test-btn')[0].attributes('disabled')).toBeUndefined()
  })

  it('shows per-proxy outcomes and allows another batch after a failure', async () => {
    testProxy.mockImplementation((id: number) => id === 1
      ? Promise.reject(new Error('offline'))
      : Promise.resolve({ success: true, country: 'GB' }))
    const wrapper = await openSelector()
    await wrapper.get('.batch-test-btn').trigger('click')
    await flushPromises()
    expect(testProxy).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('admin.proxies.testFailed')
    expect(wrapper.text()).toContain('GB')
    expect(wrapper.get('.batch-test-btn').attributes('disabled')).toBeUndefined()
    await wrapper.get('.batch-test-btn').trigger('click')
    await flushPromises()
    expect(testProxy).toHaveBeenCalledTimes(4)
  })
})

describe('managed IPv6 proxy selection', () => {
  const proxies = [
    { id: 1, name: 'manual', host: 'proxy.example', port: 8080, protocol: 'http' },
    { id: 2, name: 'current-ipv6', host: 'egress.internal', port: 1080, protocol: 'socks5', managed_ipv6: '2001:db8::2' },
    { id: 3, name: 'another-ipv6', host: 'egress.internal', port: 1080, protocol: 'socks5', managed_ipv6: '2001:db8::3' }
  ] as Proxy[]

  it('keeps the current IPv6 visible and hides other accounts IPv6 addresses', async () => {
    const wrapper = mount(ProxySelector, { props: { modelValue: 2, proxies }, global: { stubs: { Icon: true } } })
    expect(wrapper.get('.select-trigger').text()).toContain('2001:db8::2')
    await wrapper.get('.select-trigger').trigger('click')
    expect(wrapper.text()).toContain('manual')
    expect(wrapper.text()).toContain('current-ipv6')
    expect(wrapper.text()).not.toContain('another-ipv6')
    expect(wrapper.text()).not.toContain('egress.internal')
    await wrapper.get('.select-search-input').setValue('2001:db8::2')
    expect(wrapper.findAll('.test-btn')).toHaveLength(1)
    await wrapper.get('.select-search-input').setValue('2001:db8::3')
    expect(wrapper.findAll('.test-btn')).toHaveLength(0)
  })

  it('offers and tests only manual proxies for an unbound account', async () => {
    testProxy.mockResolvedValue({ success: true })
    const wrapper = mount(ProxySelector, { props: { modelValue: null, proxies }, global: { stubs: { Icon: true } } })
    await wrapper.get('.select-trigger').trigger('click')
    expect(wrapper.text()).toContain('manual')
    expect(wrapper.text()).not.toContain('current-ipv6')
    expect(wrapper.text()).not.toContain('another-ipv6')
    await wrapper.get('.batch-test-btn').trigger('click')
    await flushPromises()
    expect(testProxy.mock.calls.map(([id]) => id)).toEqual([1])
  })
})
