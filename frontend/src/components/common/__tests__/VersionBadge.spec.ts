import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { i18n, loadLocaleMessages } from '@/i18n'
import { checkUpdates, type VersionInfo } from '@/api/admin/system'
import VersionBadge from '../VersionBadge.vue'

vi.mock('@/stores', async () => ({
  useAppStore: (await import('@/stores/app')).useAppStore,
  useAuthStore: () => ({ isAdmin: true })
}))
vi.mock('@/api/admin/system', async (original) => ({
  ...await original<typeof import('@/api/admin/system')>(), checkUpdates: vi.fn()
}))

enableAutoUnmount(afterEach)
beforeEach(async () => {
  vi.mocked(checkUpdates).mockReset()
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
