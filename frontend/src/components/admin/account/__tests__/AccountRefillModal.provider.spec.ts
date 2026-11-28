import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRefillModal from '../AccountRefillModal.vue'
import type { ManualRefillRequest } from '@/api/admin/accountRefill'
import zh from '@/i18n/locales/zh/accountRefill'

const { get, manual, configure558669, catalog558669, save } = vi.hoisted(() => ({ get: vi.fn(), manual: vi.fn(), configure558669: vi.fn(), catalog558669: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountRefill', () => ({ accountRefillAPI: { get, manual, configure558669, catalog558669, save, getImportThreshold: async () => 99 } }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: async () => [{ id: 2, name: 'First', platform: 'openai', status: 'active' }, { id: 3, name: 'Second', platform: 'openai', status: 'active' }] }))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { testProxy: vi.fn() } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
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
    muze: { token_configured: true, connection_id: 's2e_fixture', products: [{ product: 'pro20x', available_now: 5, expired: false, blocked_reason: '' }] },
    supplier_558669: { token_configured: true, checked_at: new Date().toISOString(), daily: { spent_points: 12, reserved_points: 3 }, catalog: { available: 100, reserved: 8, items: [{ service: 'refill', plan: 'pro20x', price: 12, price_version: 1280, enabled: true, terms: 'fixture-terms' }] } },
    key: null, wallet: null, catalog: { items: [] }, jobs: [], daily: {}, evaluation: { accounts: [], checked_at: new Date().toISOString() }
  }
}
function pending(provider?: 'muze' | '558669', group = 2): ManualRefillRequest {
  return { request_id: `${Date.now()}:${crypto.randomUUID()}`, plan: 'pro20x', revision: 445, group_id: group, ...(provider ? { provider } : {}) }
}
async function open(initialTab: 'manual' | 'muze' | 'supplier558669') {
  const wrapper = mount(AccountRefillModal, { props: { show: true, initialTab }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' } } } })
  await flushPromises()
  return wrapper
}
async function selectTab(wrapper: Awaited<ReturnType<typeof open>>, tab: 'manual' | 'muze' | 'supplier558669') {
  await wrapper.findAll('nav button').find(b => b.text() === zh[tab])!.trigger('click')
  await flushPromises()
}
function saved(): ManualRefillRequest[] { return JSON.parse(sessionStorage.getItem(storageKey) || '[]') }

describe('Manual refill supplier isolation', () => {
  beforeEach(() => {
    sessionStorage.clear(); vi.useFakeTimers(); vi.resetAllMocks()
    get.mockImplementation(async (group?: number) => response(group))
    manual.mockImplementation(async (request: ManualRefillRequest) => response(request.group_id))
    catalog558669.mockResolvedValue(response().supplier_558669)
    configure558669.mockImplementation(async (group: number) => response(group))
    save.mockImplementation(async config => ({ ...response(config.group_id), config }))
  })
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

  it('keeps every configured supplier balance visible across rule and manual tabs', async () => {
    const w = await open('supplier558669')
    for (const tab of ['manual', 'muze', 'supplier558669'] as const) {
      await selectTab(w, tab)
      for (const supplier of ['1208', '558669', 'muze']) expect(w.get(`[data-testid="refill-wallet-${supplier}"]`).isVisible()).toBe(true)
    }
    await w.findAll('nav button').find(b => b.text() === zh.rules)!.trigger('click')
    for (const supplier of ['1208', '558669', 'muze']) expect(w.get(`[data-testid="refill-wallet-${supplier}"]`).isVisible()).toBe(true)
    expect(manual).not.toHaveBeenCalled()
    expect(save).not.toHaveBeenCalled()
    w.unmount()
  })

  it('selects 558669, shows Pro200 quote, and preserves all three uncertain requests', async () => {
    const primary = pending(), muze = pending('muze')
    sessionStorage.setItem(storageKey, JSON.stringify([primary, muze]))
    manual.mockRejectedValue({ status: 503 })
    let w = await open('supplier558669')
    expect(w.get('[data-testid="manual-refill-provider"]').text()).toContain('558669')
    expect(w.get<HTMLSelectElement>('[data-testid="manual-refill-plan"]').element.value).toBe('pro20x')
    expect(w.get('[data-testid="manual-refill-plan"]').text()).toContain('Pro200')
    expect(w.get('[data-testid="manual-refill-submit"]').text()).toContain('558669')
    expect(w.text()).toContain('1280')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    const original = structuredClone(manual.mock.calls[0]![0])
    expect(original).toMatchObject({ provider: '558669', plan: 'pro20x', group_id: 2 })
    expect(saved()).toEqual([primary, muze, original])
    w.unmount(); w = await open('supplier558669')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(original)
    expect(saved()).toEqual([primary, muze, original])
    await w.get('[data-testid="manual-refill-channel"]').setValue('manual'); await flushPromises()
    expect(w.get('[data-testid="manual-refill-provider"]').text()).toContain('1208')
    w.unmount()
  })

  it('saves 558669 secret separately, clears the input and never submits a purchase', async () => {
    const w = await open('supplier558669')
    await w.get('[data-testid="558669-token"]').setValue('fixture-558669-secret')
    expect(w.get('[data-testid="manual-refill-submit"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="558669-save"]').trigger('click'); await flushPromises()
    expect(configure558669).toHaveBeenCalledWith(2, 'fixture-558669-secret', 447)
    expect(w.get<HTMLInputElement>('[data-testid="558669-token"]').element.value).toBe('')
    expect(manual).not.toHaveBeenCalled(); expect(sessionStorage.getItem(storageKey) || '').not.toContain('fixture-558669-secret')
    w.unmount()
  })

  it('retains disabled quote reason and blocks new purchases', async () => {
    const view = response()
    const quote = { ...view.supplier_558669.catalog.items[0]!, enabled: false, reason: 'maintenance' }
    view.supplier_558669.catalog.items = [quote]
    get.mockResolvedValue(view); catalog558669.mockResolvedValue(view.supplier_558669)
    const w = await open('supplier558669')
    expect(w.text()).toContain('maintenance')
    expect(w.get('[data-testid="manual-refill-submit"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="manual-refill-force"]').attributes('disabled')).toBeDefined()
    expect(manual).not.toHaveBeenCalled()
    w.unmount()
  })

  it('saves 558669 primary and an explicit 1208 backup without a Muze connection', async () => {
    const view = response(); view.muze.token_configured = false
    get.mockResolvedValue(view)
    const w = await open('supplier558669')
    await w.get('[data-testid="refill-primary-provider"]').setValue('558669')
    await w.get('[data-testid="refill-backup-provider"]').setValue('1208')
    await w.get('[data-testid="refill-muze-fallback-enabled"]').setValue(true)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0]![0]).toMatchObject({ primary_provider: '558669', muze_fallback: { provider: '1208', enabled: true }, supplier_558669: { max_unit_price: 60, daily_max_points: 3000 } })
    expect(manual).not.toHaveBeenCalled()
    w.unmount()
  })

  it('archives retired browser receipts without replaying them on either supplier', async () => {
    const old = { ...pending(), provider: 'fendudu' }
    sessionStorage.setItem(storageKey, JSON.stringify([old]))
    const w = await open('muze')
    expect(manual).not.toHaveBeenCalled()
    expect(JSON.parse(sessionStorage.getItem(storageKey + ':retired') || '[]')).toEqual([old])
    await w.get('[data-testid="manual-refill-submit"]').trigger('click')
    await flushPromises()
    expect(manual.mock.calls[0]![0].provider).toBe('muze')
    expect(manual.mock.calls[0]![0].request_id).not.toBe(old.request_id)
    w.unmount()
  })

  it.each([
    ['manual', false], ['manual', true], ['muze', false], ['muze', true]
  ] as const)('keeps explicit %s entry with an opposite supplier legacy request, force=%s', async (tab, force) => {
    const original = pending(tab === 'manual' ? 'muze' : undefined)
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    const w = await open(tab)
    const supplier = tab === 'muze' ? 'Muze' : '1208'
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh[tab])
    expect(w.get('[data-testid="manual-refill-provider"]').text()).toContain(supplier)
    expect(w.get('[data-testid="manual-other-provider-pending"]').text()).toContain(tab === 'manual' ? 'Muze' : '1208')
    const button = w.get(`[data-testid="manual-refill-${force ? 'force' : 'submit'}"]`)
    expect(button.text()).toContain(supplier)
    expect(button.attributes('disabled')).toBeUndefined()
    await button.trigger('click'); await flushPromises()
    expect(manual).toHaveBeenCalledTimes(1)
    const submitted = manual.mock.calls[0]![0]
    expect(submitted.provider).toBe(tab === 'muze' ? 'muze' : undefined)
    expect(!!submitted.force).toBe(force)
    expect(submitted.request_id).not.toBe(original.request_id)
    expect(saved()).toEqual([original])
    expect(sessionStorage.getItem(legacyKey)).toBeNull()
    w.unmount()
    const reopened = await open(tab === 'manual' ? 'muze' : 'manual')
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
    await selectTab(w, 'muze')
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    const muze = structuredClone(manual.mock.calls[1]![0])
    expect(saved()).toEqual([primary, muze])
    w.unmount(); w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(primary)
    await selectTab(w, 'muze')
    expect(w.get('[data-testid="manual-refill-submit"]').text()).toBe('重试确认Muze原请求')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(muze)
    expect(muze).toMatchObject({ provider: 'muze', force: true })
    w.unmount()
  })

  it('removes only the rejected supplier request and retains the selected entry for the next force', async () => {
    const primary = pending(), muze = { ...pending('muze'), force: true }
    sessionStorage.setItem(storageKey, JSON.stringify([primary, muze]))
    manual.mockRejectedValueOnce({ status: 400 })
    const w = await open('manual')
    await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(primary)
    expect(saved()).toEqual([muze])
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh.manual)
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    expect(manual.mock.calls[1]![0]).toMatchObject({ force: true, plan: primary.plan })
    expect(manual.mock.calls[1]![0].provider).toBeUndefined()
    expect(saved()).toEqual([muze])
    w.unmount()
  })

  it('isolates the same supplier requests by group and loads the original request group on reopen', async () => {
    const group3 = pending('muze', 3)
    sessionStorage.setItem(storageKey, JSON.stringify([group3]))
    const w = await open('manual')
    expect(w.get<HTMLSelectElement>('[data-testid="refill-rule-group"]').element.value).toBe('2')
    await selectTab(w, 'muze')
    await w.get('[data-testid="manual-refill-force"]').trigger('click'); await flushPromises()
    expect(manual.mock.calls[0]![0]).toMatchObject({ provider: 'muze', group_id: 2, force: true })
    expect(saved()).toEqual([group3])
    w.unmount()
    const reopened = await open('muze')
    expect(reopened.get<HTMLSelectElement>('[data-testid="refill-rule-group"]').element.value).toBe('3')
    await reopened.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
    expect(manual).toHaveBeenLastCalledWith(group3)
    reopened.unmount()
  })

  it('clears a confirmed opposite supplier receipt without redirecting the normal entry', async () => {
    const original = pending('muze')
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    get.mockResolvedValue({ ...response(), jobs: [{ id: 'known', provider: 'muze', manual_request_id: original.request_id, status: 'submitting' }] })
    const w = await open('manual')
    expect(w.get('nav button[aria-pressed="true"]').text()).toBe(zh.manual)
    expect(saved()).toEqual([])
    expect(manual).not.toHaveBeenCalled()
    w.unmount()
  })

  it('blocks purchase if migrating the legacy request cannot be saved', async () => {
    const original = pending('muze')
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
    const original = pending('muze'); delete original.group_id
    sessionStorage.setItem(legacyKey, JSON.stringify(original))
    const w = await open('muze')
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
