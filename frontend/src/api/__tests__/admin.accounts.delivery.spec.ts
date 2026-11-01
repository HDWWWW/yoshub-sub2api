import { beforeEach, describe, expect, it, vi } from 'vitest'
const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))
import { importData, setPrivacy } from '@/api/admin/accounts'

beforeEach(() => { post.mockReset(); post.mockResolvedValue({ data: {} }) })

describe('account delivery API payloads', () => {
  it('forwards request-level proxy overrides through the data import wrapper', async () => {
    const data = { proxies: [], accounts: [] }
    await importData({ data, proxy_mode: 'ovh_ipv6', proxy_id: null, skip_default_group_bind: true })
    expect(post).toHaveBeenCalledWith('/admin/accounts/data', { data, proxy_mode: 'ovh_ipv6', proxy_id: null, skip_default_group_bind: true })
  })

  it('keeps the explicit false privacy preference and the legacy default distinct', async () => {
    await setPrivacy(7, false)
    expect(post).toHaveBeenLastCalledWith('/admin/accounts/7/set-privacy', { enabled: false })
    await setPrivacy(7)
    expect(post.mock.calls.at(-1)?.[1]).toBeUndefined()
  })
})
