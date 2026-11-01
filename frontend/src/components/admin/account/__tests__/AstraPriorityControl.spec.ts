import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import AstraPriorityControl from '../AstraPriorityControl.vue'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/astraPriority', () => ({ astraPriorityAPI: { get, save } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
beforeEach(() => { vi.resetAllMocks(); get.mockResolvedValue({ enabled: true }) })

describe('6a priority fill setting', () => {
  it('loads persisted state and disables repeat writes while saving', async () => {
    let finish!: (value: { enabled: boolean }) => void
    save.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mount(AstraPriorityControl)
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    await wrapper.get('[role="switch"]').trigger('click')
    expect(save).toHaveBeenCalledWith(false)
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    finish({ enabled: false }); await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeUndefined()
  })

  it('retains the previous state and reports save failure', async () => {
    save.mockRejectedValue(new Error('offline'))
    const wrapper = mount(AstraPriorityControl); await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.get('[role="alert"]').exists()).toBe(true)
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeUndefined()
  })

  it('does not present a failed initial read as a writable false setting', async () => {
    get.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mount(AstraPriorityControl); await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    expect(save).not.toHaveBeenCalled()
    await wrapper.get('[role="alert"] button').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })
})
