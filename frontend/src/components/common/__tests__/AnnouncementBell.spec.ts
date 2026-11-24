import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import AnnouncementBell from '../AnnouncementBell.vue'
import { useAnnouncementStore } from '@/stores/announcements'

const { markRead, showError, showSuccess } = vi.hoisted(() => ({ markRead: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api', () => ({ announcementsAPI: { markRead } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/utils/format', () => ({ formatRelativeTime: () => 'now', formatRelativeWithDateTime: () => 'now' }))
enableAutoUnmount(afterEach)
beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  vi.spyOn(console, 'error').mockImplementation(() => {})
  useAnnouncementStore().announcements = [{ id: 1, title: 'Notice', content: 'Details', notify_mode: 'silent', created_at: '2026-11-24T23:20:39Z', updated_at: '2026-09-30' }]
})
afterEach(() => { vi.restoreAllMocks(); document.body.style.overflow = '' })

describe('announcement read confirmation', () => {
  it('keeps a failed read confirmation open and only reports success after retry', async () => {
    markRead.mockRejectedValue(new Error('offline'))
    const wrapper = mount(AnnouncementBell, { global: { stubs: { Teleport: true, Transition: true, Icon: true } } })
    await wrapper.get('button').trigger('click')
    expect(wrapper.get('time').attributes('title')).toBe('2026-11-25 07:20:39 (UTC+08:00)')
    await wrapper.get('.group.relative').trigger('click')
    await flushPromises()
    const confirm = () => wrapper.findAll('button').find(button => button.text() === 'announcements.markRead')!
    await confirm().trigger('click')
    await flushPromises()
    expect(markRead).toHaveBeenCalledTimes(2)
    expect(showSuccess).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalled()
    expect(wrapper.find('.markdown-body').exists()).toBe(true)
    markRead.mockResolvedValue(undefined)
    await confirm().trigger('click')
    await flushPromises()
    expect(showSuccess).toHaveBeenCalledWith('announcements.markedAsRead')
    expect(wrapper.find('.markdown-body').exists()).toBe(false)
    expect(useAnnouncementStore().unreadCount).toBe(0)
  })
})
