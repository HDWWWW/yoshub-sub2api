import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRefillModal from '../AccountRefillModal.vue'
import type { ManualRefillRequest } from '@/api/admin/accountRefill'
import zh from '@/i18n/locales/zh/accountRefill'

const { get, manual } = vi.hoisted(() => ({ get: vi.fn(), manual: vi.fn() }))
vi.mock('@/api/admin/accountRefill', () => ({ accountRefillAPI: { get, manual } }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: async () => [{ id: 2, name: 'First', platform: 'openai', status: 'active' }, { id: 3, name: 'Second', platform: 'openai', status: 'active' }] }))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { testProxy: vi.fn() } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn() }) }))
vi.mock('@/components/auth/TotpStepUpDialog.vue', () => ({ default: { template: '<div />' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ te: () => true, t: (key: string, values: Record<string, string> = {}) => {
  const text = zh[key.replace('accountRefill.', '') as keyof typeof zh]
  return typeof text === 'string' ? text.replace(/\{(\w+)\}/g, (_, name: string) => values[name] || '') : key
} }) }))

const legacyKey = 'yoshub:manual-refill:pending'
const storageKey = `${legacyKey}:v2`
function response(group = 2) {
  return {
    config: { group_id: group, delivery_group_ids: [group], enabled: false, monitor_enabled: false, push_enabled: true,
      plan: 'pro20x', rotation: [], import_concurrency: 60, notifications: {},
      delivery: { model_mapping: {}, fingerprint_mode: 'full', proxy_mode: 'direct', expiry_mode: 'rental' } },
    revision: 447, rules: [{ group_id: 2 }], token_configured: true,
    fendudu: { token_configured: true, connection_id: 's2e_fixture', products: [{ product: 'pro20x', available_now: 5, expired: false, blocked_reason: '' }] },
    key: null, wallet: null, catalog: { items: [] }, jobs: [], daily: {}, evaluation: { accounts: [], checked_at: new Date().toISOString() }
  }
}
function pending(provider?: 'fendudu', group = 2): ManualRefillRequest {
  return { request_id: `${Date.now()}:${crypto.randomUUID()}`, plan: 'pro20x', revision: 445, group_id: group, ...(provider ? { provider } : {}) }
}
async function open(initialTab: 'manual' | 'fendudu') {
  const wrapper = mount(AccountRefillModal, { props: { show: true, initialTab }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' } } } })
  await flushPromises()
  return wrapper
}
async function selectTab(wrapper: Awaited<ReturnType<typeof open>>, tab: 'manual' | 'fendudu') {
  await wrapper.findAll('nav button').find(b => b.text() === zh[tab])!.trigger('click')
  await flushPromises()
}
function saved(): ManualRefillRequest[] { return JSON.parse(sessionStorage.getItem(storageKey) || '[]') }

describe('Manual refill supplier isolation', () => {
  beforeEach(() => {
    sessionStorage.clear(); vi.useFakeTimers(); vi.resetAllMocks()
    get.mockImplementation(async (group?: number) => response(group))
    manual.mockImplementation(async (request: ManualRefillRequest) => response(request.group_id))
  })
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

  it.each([
    ['manual', false], ['manual', true], ['fendudu', false], ['fendudu', true]
  ] as const)('keeps explicit %s entry with an opposite supplier legacy request, force=%s', async (tab, force) => {
    const original = pending(tab === 'manual' ? 'fendudu' : undefined)
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    const w = await open(tab)
    const supplier = tab === 'fendudu' ? '粉嘟嘟' : '原号商'
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh[tab])
    expect(w.get('[data-testid="manual-refill-provider"]').text()).toContain(supplier)
    expect(w.get('[data-testid="manual-other-provider-pending"]').text()).toContain(tab === 'manual' ? '粉嘟嘟' : '原号商')
    const button = w.get(`[data-testid="manual-refill-${force ? 'force' : 'submit'}"]`)
    expect(button.text()).toContain(supplier)
    expect(button.attributes('disabled')).toBeUndefined()
    await button.trigger('click'); await flushPromises()
    expect(manual).toHaveBeenCalledTimes(1)
    const submitted = manual.mock.calls[0]![0]
    expect(submitted.provider).toBe(tab === 'fendudu' ? 'fendudu' : undefined)
    expect(!!submitted.force).toBe(force)
    expect(submitted.request_id).not.toBe(original.request_id)
    expect(saved()).toEqual([original])
    expect(sessionStorage.getItem(legacyKey)).toBeNull()
    w.unmount()
    const reopened = await open(tab === 'manual' ? 'fendudu' : 'manual')
    expect(reopened.get('[data-testid="manual-refill-submit"]').text()).toContain('重试确认')
    await reopened.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(original)
    reopened.unmount()
  })

  it('retains both suppliers after timeouts, switching tabs and reopening', async () => {
    manual.mockRejectedValue({ status: 503 })
    let w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    const primary = structuredClone(manual.mock.calls[0]![0])
    await selectTab(w, 'fendudu')
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    const fendudu = structuredClone(manual.mock.calls[1]![0])
    expect(saved()).toEqual([primary, fendudu])
    w.unmount(); w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(primary)
    await selectTab(w, 'fendudu')
    expect(w.get('[data-testid="manual-refill-submit"]').text()).toBe('重试确认粉嘟嘟原请求')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(fendudu)
    expect(fendudu).toMatchObject({ provider: 'fendudu', force: true })
    w.unmount()
  })

  it('removes only the rejected supplier request and retains the selected entry for the next force', async () => {
    const primary = pending(), fendudu = { ...pending('fendudu'), force: true }
    sessionStorage.setItem(storageKey, JSON.stringify([primary, fendudu]))
    manual.mockRejectedValueOnce({ status: 400 })
    const w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(primary)
    expect(saved()).toEqual([fendudu])
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh.manual)
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    expect(manual.mock.calls[1]![0]).toMatchObject({ force: true })
    expect(manual.mock.calls[1]![0].provider).toBeUndefined()
    expect(saved()).toEqual([fendudu])
    w.unmount()
  })

  it('isolates the same supplier requests by group and loads the original request group on reopen', async () => {
    const group3 = pending('fendudu', 3)
    sessionStorage.setItem(storageKey, JSON.stringify([group3]))
    const w = await open('manual')
    expect(w.get<HTMLSelectElement>('[data-testid="refill-rule-group"]').element.value).toBe('2')
    await selectTab(w, 'fendudu')
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    expect(manual.mock.calls[0]![0]).toMatchObject({ provider: 'fendudu', group_id: 2, force: true })
    expect(saved()).toEqual([group3])
    w.unmount()
    const reopened = await open('fendudu')
    expect(reopened.get<HTMLSelectElement>('[data-testid="refill-rule-group"]').element.value).toBe('3')
    await reopened.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(group3)
    reopened.unmount()
  })

  it('clears a confirmed opposite supplier receipt without redirecting the normal entry', async () => {
    const original = pending('fendudu')
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    get.mockResolvedValue({ ...response(), jobs: [{ id: 'known', provider: 'fendudu', manual_request_id: original.request_id, status: 'submitting' }] })
    const w = await open('manual')
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh.manual)
    expect(saved()).toEqual([])
    expect(manual).not.toHaveBeenCalled()
    w.unmount()
  })

  it('blocks purchase if migrating the legacy request cannot be saved', async () => {
    const original = pending('fendudu')
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    const w = await open('manual')
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('quota exceeded') })
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).not.toHaveBeenCalled()
    expect(sessionStorage.getItem(legacyKey)).toBe(JSON.stringify(original))
    expect(w.text()).toContain(zh.manualStorageError)
    w.unmount()
  })

  it.each(['{invalid', 'null', '[{"request_id":"invalid"}]'])('blocks both purchase buttons on unreadable pending state %s', async raw => {
    sessionStorage.setItem(storageKey, raw)
    const w = await open('manual')
    expect(w.get('[data-testid="manual-refill-submit"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="manual-refill-force"]').attributes('disabled')).toBeDefined()
    expect(manual).not.toHaveBeenCalled()
    w.unmount()
  })

  it('keeps a legacy request without group bound to the default group', async () => {
    const original = pending('fendudu'); delete original.group_id
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    const w = await open('fendudu')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenCalledWith({ ...original, group_id: 2 })
    w.unmount()
  })

  it('locks supplier tabs while a purchase response is pending', async () => {
    let resolve!: (value: ReturnType<typeof response>) => void
    manual.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click')
    expect(w.findAll('nav button').every(button => button.attributes('disabled') !== undefined)).toBe(true)
    resolve(response()); await flushPromises()
    expect(manual.mock.calls[0]![0].provider).toBeUndefined()
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh.progress)
    w.unmount()
  })
})
