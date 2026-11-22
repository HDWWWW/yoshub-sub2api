import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { i18n, loadLocaleMessages } from '@/i18n'
import { checkUpdates, checkUpstreamRelease, performUpdate, rollback, type VersionInfo } from '@/api/admin/system'
import VersionBadge from '../VersionBadge.vue'

const auth = vi.hoisted(() => ({ isAdmin: true }))

vi.mock('@/stores', async () => ({
  useAppStore: (await import('@/stores/app')).useAppStore,
  useAuthStore: () => auth
}))
vi.mock('@/api/admin/system', async (original) => ({
  ...await original<typeof import('@/api/admin/system')>(), checkUpdates: vi.fn(),
  checkUpstreamRelease: vi.fn(), performUpdate: vi.fn(), rollback: vi.fn()
}))

enableAutoUnmount(afterEach)
beforeEach(async () => {
  auth.isAdmin = true
  vi.clearAllMocks()
  vi.mocked(checkUpdates).mockReset()
  vi.mocked(checkUpstreamRelease).mockReset().mockResolvedValue({ base_version: 'v0.2.24', latest_version: 'v0.2.24', status: 'current' })
  await loadLocaleMessages('zh')
  i18n.global.locale.value = 'zh'
})

const available: VersionInfo = {
  current_version: '0.2.23-yoshub.47', latest_version: '0.2.23-yoshub.48',
  has_update: true, self_update_enabled: true, can_update: true, build_type: 'release', cached: false,
  release_info: {
    name: 'YosHub', body: '', published_at: '',
    html_url: 'https://github.com/HDWWWW/yoshub-sub2api/releases/tag/image-v0.2.23-yoshub.48'
  }
}

async function openPanel() {
  const pinia = createPinia()
  setActivePinia(pinia)
  const wrapper = mount(VersionBadge, { global: { plugins: [pinia, i18n], stubs: { Icon: true } } })
  await flushPromises()
  await wrapper.get('button').trigger('click')
  return wrapper
}

describe('YosHub version menu', () => {
  it('offers the available binary and links to the release for this fork', async () => {
    vi.mocked(checkUpdates).mockResolvedValue(available)
    const panel = await openPanel()
    expect(panel.text()).toContain('立即更新')
    expect(panel.get('a[href$="/releases"]').text()).toContain('YosHub')
    expect(panel.get('a[href*="/tag/"]').attributes('href')).toBe(available.release_info?.html_url)
  })

  it('offers manual installation when the release only contains an image', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({ ...available, can_update: false })
    const panel = await openPanel()
    expect(panel.text()).toContain('没有适用于当前平台的热更新包')
    expect(panel.text()).not.toContain('立即更新')
  })

  it.each(['cached', 'network'])('shows a retry after a %s check failure', async (failure) => {
    if (failure === 'cached') {
      vi.mocked(checkUpdates).mockResolvedValue({ ...available, can_update: false, warning: 'GitHub unavailable', cached: true })
    } else {
      vi.mocked(checkUpdates).mockRejectedValue(new Error('GitHub unavailable'))
    }
    const panel = await openPanel()
    expect(panel.text()).toContain('暂时无法检查更新')
    expect(panel.text()).not.toContain('已是最新版本')
    expect(panel.text()).not.toContain('立即更新')
    vi.mocked(checkUpdates).mockResolvedValue(available)
    await panel.get('[role="status"] button').trigger('click')
    await flushPromises()
    expect(panel.text()).toContain('立即更新')
  })
})


describe('read-only upstream release notice', () => {
  it.each([
    ['current', 'v0.2.24', '已跟上原版'],
    ['available', 'v0.2.25', '原版有更新'],
    ['unavailable', undefined, '暂时无法确认原版版本']
  ] as const)('shows %s independently of fork updates', async (status, latest_version, text) => {
    vi.mocked(checkUpdates).mockResolvedValue({ ...available, has_update: false, can_update: false })
    vi.mocked(checkUpstreamRelease).mockResolvedValue({ base_version: 'v0.2.24', latest_version, status })
    const panel = await openPanel()
    await flushPromises()
    const section = panel.get('section[aria-label="原版 Sub2API"]')
    expect(section.text()).toContain(text)
    expect(section.text()).toContain('本版基于 v0.2.24')
    if (latest_version) expect(section.text()).toContain(latest_version)
    expect(section.findAll('button, a, input')).toHaveLength(0)
    expect(panel.text()).not.toContain('立即更新')
    expect(performUpdate).not.toHaveBeenCalled()
    expect(rollback).not.toHaveBeenCalled()
  })

  it('keeps the fork actions usable during and after upstream failure', async () => {
    vi.mocked(checkUpdates).mockResolvedValue(available)
    let fail!: (reason: Error) => void
    vi.mocked(checkUpstreamRelease).mockReturnValue(new Promise((_resolve, reject) => { fail = reject }))
    const panel = await openPanel()
    expect(panel.text()).toContain('正在检查原版更新')
    expect(panel.text()).toContain('立即更新')
    fail(new Error('upstream offline'))
    await flushPromises()
    expect(panel.text()).toContain('暂时无法确认原版版本')
    expect(panel.text()).toContain('立即更新')
    vi.mocked(checkUpstreamRelease).mockResolvedValue({ base_version: 'v0.2.24', latest_version: 'v0.2.25', status: 'available' })
    await panel.get('button[title="刷新"]').trigger('click')
    await flushPromises()
    expect(checkUpstreamRelease).toHaveBeenLastCalledWith(true)
    expect(panel.text()).toContain('原版有更新')
  })

  it('does not check the original project for non-admins or a closed menu', async () => {
    vi.mocked(checkUpdates).mockResolvedValue(available)
    const pinia = createPinia()
    setActivePinia(pinia)
    const options = { props: { version: '0.2.24-yoshub.50' }, global: { plugins: [pinia, i18n], stubs: { Icon: true } } }
    mount(VersionBadge, options)
    await flushPromises()
    expect(checkUpstreamRelease).not.toHaveBeenCalled()
    auth.isAdmin = false
    const panel = mount(VersionBadge, options)
    await flushPromises()
    expect(panel.text()).toBe('v0.2.24-yoshub.50')
    expect(panel.find('button').exists()).toBe(false)
    expect(checkUpstreamRelease).not.toHaveBeenCalled()
  })
})
