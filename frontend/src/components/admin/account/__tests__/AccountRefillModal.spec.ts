import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRefillModal from '../AccountRefillModal.vue'
import type { Proxy } from '@/types'

const { get, save, check, retry, manual } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn(), check: vi.fn(), retry: vi.fn(), manual: vi.fn() }))
vi.mock('@/api/admin/accountRefill', () => ({ accountRefillAPI: { get, save, check, retry, manual } }))
vi.mock('@/api/admin', () => ({ adminAPI: { proxies: { testProxy: vi.fn() } } }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: async () => [{ id: 2, name: 'OpenAI', platform: 'openai', status: 'active' }, { id: 3, name: 'Second group', platform: 'openai', status: 'active' }] }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
const config = {
 notifications: { refill_started: false, low_balance: false, low_balance_threshold: 100 },
 enabled: false, monitor_enabled: true, mode: 'capacity', group_id: 2, delivery_group_ids: [2], push_enabled: true,
 max_in_flight: 1, interval_minutes: 60, interval_quantity: 1, plan: 'pro5x', min_ready: 1, max_ready: 20,
 lead_minutes: 30, min_remaining_5h: 15, min_remaining_7d: 15, target_concurrency_percent: 85,
 concurrency_sustain_seconds: 30, import_expiry_grace_minutes: 60,
 check_seconds: 60, cooldown_seconds: 300, daily_max_count: 50, daily_max_points: 3000, max_unit_price: 60,
 import_concurrency: 60, window_enabled: false, window_start: '09:00', window_end: '23:00',
 delivery: { ws_mode: 'http_bridge', proxy_mode: 'direct', proxy_id: null, name_template: '', notes_template: '', priority: 1, load_factor: 0, rate_multiplier: 1, status: 'active', schedulable: true, auto_pause_on_expired: true, fingerprint_mode: 'full', expiry_mode: 'rental', fixed_expires_at: null, model_mapping: {} }
}
function response(revision = 1) { return { config: structuredClone(config), revision, token_configured: true, key: { label: 'key label', scope: 'write', expires_at: '2027-01-29T12:59:37Z' }, wallet: { balance: 4825.5, held: 60, available: 4765.5 }, identity_at: '2026-10-31T12:00:00Z', catalog: { items: [{ service: 'refill', plan: 'pro5x', price: 60, enabled: true }] }, jobs: [], evaluation: { ready: 1, accounts: [] }, daily: {}, message: 'ready' } }
async function open() { const wrapper = mount(AccountRefillModal, { props: { show: true }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' } } } }); await flushPromises(); return wrapper }
function responseWithJobs(count: number) {
 return { ...response(), jobs: Array.from({ length: count }, (_, i) => ({
  id: `order-${i + 1}`, plan: 'pro5x', price: 60, status: 'imported', reason: '账号临期，提前补充储备',
  reason_detail: '未来健康储备 0 个 + 在途 0 个，低于最低储备 1 个。未计入储备：1 个将在 30 分钟内到期。',
  created_at: '2026-11-01T04:00:00Z', updated_at: '2026-11-01T04:01:00Z', finished_at: '2026-11-01T04:01:00Z',
  attempts: 1, next_attempt_at: '2026-11-01T04:00:00Z'
 })) }
}
describe('AccountRefillModal', () => {
 beforeEach(() => { sessionStorage.clear(); vi.useFakeTimers(); vi.resetAllMocks(); get.mockResolvedValue(response()); save.mockImplementation(async (cfg) => ({ ...response(2), config: cfg })); check.mockResolvedValue(response()); manual.mockResolvedValue(response()) })
 afterEach(() => vi.useRealTimers())
 it('keeps Fast edits while a save waits and restores them after reopening', async () => {
  const w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.autoFast')!.trigger('click')
  await w.get('[data-testid="refill-auto-fast-enabled"]').setValue(true)
  await w.get('[data-testid="refill-auto-fast-follow-lead"]').setValue(false)
  await w.get('[data-testid="refill-auto-fast-lead"]').setValue(10)
  await w.get('[data-testid="refill-auto-fast-start-remaining"]').setValue(40)
  let complete!: (value: unknown) => void
  save.mockImplementationOnce(() => new Promise(resolve => { complete = resolve }))
  await w.get('form').trigger('submit'); await flushPromises()
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined()
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect((w.get('[data-testid="refill-auto-fast-enabled"]').element as HTMLInputElement).checked).toBe(true)
  expect((w.get('[data-testid="refill-auto-fast-lead"]').element as HTMLInputElement).value).toBe('10')
  const persisted = { ...response(2), config: structuredClone(save.mock.calls[0]![0]) }
  complete(persisted); await flushPromises()
  expect(save).toHaveBeenCalledTimes(1)
  expect(check).not.toHaveBeenCalled(); expect(manual).not.toHaveBeenCalled()
  w.unmount()
  get.mockResolvedValue(persisted)
  const reopened = await open()
  await reopened.findAll('nav button').find(b => b.text() === 'accountRefill.autoFast')!.trigger('click')
  expect((reopened.get('[data-testid="refill-auto-fast-enabled"]').element as HTMLInputElement).checked).toBe(true)
  expect((reopened.get('[data-testid="refill-auto-fast-lead"]').element as HTMLInputElement).value).toBe('10')
  expect((reopened.get('[data-testid="refill-auto-fast-start-remaining"]').element as HTMLInputElement).value).toBe('40')
  expect(reopened.get('[data-testid="refill-auto-fast-follow"]').attributes('aria-pressed')).toBe('true')
  reopened.unmount()
 })
 it('saves independent automatic Fast start conditions and custom used-quota limits', async () => {
  const w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.autoFast')!.trigger('click')
  const panel = w.get('[data-testid="refill-auto-fast-panel"]')
  expect(panel.isVisible()).toBe(true)
  expect(panel.text()).toContain('accountRefill.autoFastBilling')
  expect((w.get('[data-testid="refill-auto-fast-enabled"]').element as HTMLInputElement).checked).toBe(false)
  await w.get('[data-testid="refill-auto-fast-enabled"]').setValue(true)
  await w.get('[data-testid="refill-auto-fast-follow-lead"]').setValue(false)
  await w.get('[data-testid="refill-auto-fast-lead"]').setValue(10)
  await w.get('[data-testid="refill-auto-fast-start-remaining"]').setValue(40)
  await w.get('[data-testid="refill-auto-fast-custom"]').trigger('click')
  await w.get('[data-testid="refill-auto-fast-used-5h"]').setValue(85)
  await w.get('[data-testid="refill-auto-fast-used-7d"]').setValue(90)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save.mock.calls[0]![0].auto_fast).toEqual({ enabled: true, lead_minutes: 10, start_remaining_percent: 40, stop_mode: 'custom', stop_used_5h_percent: 85, stop_used_7d_percent: 90 })
  expect(save.mock.calls[0]![0].enabled).toBe(false)
  expect(save.mock.calls[0]![0].min_remaining_5h).toBe(15)
  expect(check).not.toHaveBeenCalled(); expect(manual).not.toHaveBeenCalled()
  await w.get('[data-testid="refill-auto-fast-follow"]').trigger('click')
  await w.get('[data-testid="refill-auto-fast-follow-lead"]').setValue(true)
  await w.get('[data-testid="refill-auto-fast-enabled"]').setValue(false)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save.mock.calls[1]![0].auto_fast).toMatchObject({ enabled: false, lead_minutes: null, stop_mode: 'follow' })
  w.unmount()
 })
 it('offers only manual proxies for specified replenishment and saves a real selected ID', async () => {
  const w = await open()
  await w.setProps({ proxies: [
   { id: 1, name: 'manual proxy', host: 'manual', port: 8080, protocol: 'http' },
   { id: 2, name: 'reserved IPv6', host: 'internal', port: 8080, protocol: 'http', managed_ipv6: '2001:db8::2', managed_ipv6_state: 'reserved' },
   { id: 3, name: 'bound IPv6', host: 'internal', port: 8080, protocol: 'http', managed_ipv6: '2001:db8::3', managed_ipv6_state: 'bound' }
  ] as Proxy[] })
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.delivery')!.trigger('click')
  const proxy = w.findAll('select').find(s => s.find('option[value="specified"]').exists())!
  await proxy.setValue('specified')
  await w.get('.select-trigger').trigger('click')
  expect(w.text()).toContain('manual proxy')
  expect(w.text()).not.toContain('reserved IPv6')
  expect(w.text()).not.toContain('bound IPv6')
  await w.findAll('.select-option').find(option => option.text().includes('manual proxy'))!.trigger('click')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ delivery: expect.objectContaining({ proxy_mode: 'specified', proxy_id: 1 }) }), '', 1, '')
  w.unmount()
 })
 it('defaults legacy delivery privacy on and persists false with explicit OVH allocation', async () => {
  const w = await open()
  const privacy = w.get('[data-testid="openai-privacy-enabled"]')
  expect((privacy.element as HTMLInputElement).checked).toBe(true)
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.delivery')!.trigger('click')
  await privacy.setValue(false)
  const proxy = w.findAll('select').find(s => s.find('option[value="ovh_ipv6"]').exists())!
  await proxy.setValue('ovh_ipv6')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ delivery: expect.objectContaining({ privacy_enabled: false, proxy_mode: 'ovh_ipv6', proxy_id: null }) }), '', 1, '')
  expect((privacy.element as HTMLInputElement).checked).toBe(false)
  expect(manual).not.toHaveBeenCalled()
  w.unmount()
 })
 it('separates live utilization from departure observation without submitting an order', async () => {
  const observation = { since: null, last_sample_at: '2026-11-01T04:00:00Z', sustained_seconds: 30, required_seconds: 30, samples: 31, no_capacity: false }
  const data = {
   ...response(),
   capacity_observation: { ...observation, status: 'below', ready: false, utilization_percent: 45 },
   forecast_observation: { ...observation, status: 'ready', ready: true, utilization_percent: 90 },
   evaluation: { ...response().evaluation, in_use: 90, capacity: 200, forecast_capacity: 100, pending_capacity: 0, forecast_accounts: 1,
    accounts: [{ id: 7, name: 'reaching floor', state: 'ready', forecast_loss: true }, { id: 8, name: 'healthy reserve', state: 'ready', forecast_loss: false }] }
  }
  get.mockResolvedValue(data)
  const w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.progress')!.trigger('click')
  const live = w.get('[data-testid="refill-current-observation"]')
  const forecast = w.get('[data-testid="refill-forecast-observation"]')
  expect(live.text()).toContain('accountRefill.observation_below')
  expect(forecast.text()).toContain('accountRefill.observation_ready')
  expect(forecast.text()).toContain('accountRefill.forecastProgress')
  expect(forecast.text()).toContain('#7 reaching floor')
  expect(forecast.text()).not.toContain('#8 healthy reserve')
  get.mockResolvedValue({ ...data, forecast_observation: { ...data.forecast_observation, no_capacity: true } })
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect(forecast.text()).toContain('accountRefill.observationNoCapacity')
  expect(manual).not.toHaveBeenCalled()
  expect(check).not.toHaveBeenCalled()
  expect(save).not.toHaveBeenCalled()
  w.unmount()
 })
 it('defaults legacy forecast rules on and saves explicit disable and re-enable independently', async () => {
  const w = await open()
  const toggle = w.get('[data-testid="refill-forecast-enabled"]')
  expect((toggle.element as HTMLInputElement).checked).toBe(true)
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined()
  await toggle.setValue(false)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ forecast_enabled: false, monitor_enabled: true, concurrency_sustain_seconds: 30, min_ready: 1 }), '', 1, '')
  await toggle.setValue(true)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ forecast_enabled: true }), '', 2, '')
  expect(check).not.toHaveBeenCalled(); expect(manual).not.toHaveBeenCalled()
  w.unmount()
 })
 it('shows disabled forecast status without stale statistics while keeping live observation visible', async () => {
  const observation = { since: null, last_sample_at: '2026-11-01T04:00:00Z', sustained_seconds: 30, required_seconds: 30, samples: 31, no_capacity: false, status: 'ready', ready: true, utilization_percent: 90 }
  get.mockResolvedValue({ ...response(), config: { ...structuredClone(config), forecast_enabled: false }, capacity_observation: observation, forecast_observation: observation,
    evaluation: { ...response().evaluation, forecast_accounts: 1, forecast_capacity: 100, accounts: [{ id: 7, name: 'stale forecast account', forecast_loss: true }] } })
  const w = await open()
  expect((w.get('[data-testid="refill-forecast-enabled"]').element as HTMLInputElement).checked).toBe(false)
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.progress')!.trigger('click')
  const forecast = w.get('[data-testid="refill-forecast-observation"]')
  expect(forecast.text()).toContain('accountRefill.observation_disabled')
  expect(forecast.text()).not.toContain('accountRefill.forecastProgress')
  expect(forecast.text()).not.toContain('accountRefill.forecastCapacity')
  expect(forecast.text()).not.toContain('stale forecast account')
  expect(forecast.text()).not.toContain('accountRefill.lastSample')
  expect(w.get('[data-testid="refill-current-observation"]').text()).toContain('accountRefill.observation_ready')
  w.unmount()
 })
 it('shows saved trigger notes and handles historical records without inventing a reason', async () => {
  const data = responseWithJobs(3)
  data.jobs[1]!.reason = '并发利用率超过目标'; data.jobs[1]!.reason_detail = ''
  data.jobs[2]!.reason = ''; data.jobs[2]!.reason_detail = ''
  get.mockResolvedValue(data)
  const w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.progress')!.trigger('click')
  const reasons = w.findAll('[data-testid="refill-job-reason"]')
  expect(reasons[0]!.text()).toContain('1 个将在 30 分钟内到期')
  expect(reasons[1]!.text()).toBe('并发利用率超过目标')
  expect(reasons[2]!.text()).toBe('accountRefill.reasonUnknown')
  expect(manual).not.toHaveBeenCalled()
  w.unmount()
 })
 it('paginates ten orders, keeps the selected page on refresh, and clamps shrinking history', async () => {
  get.mockResolvedValue(responseWithJobs(23))
  const w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.progress')!.trigger('click')
  const rows = () => w.findAll('[data-testid="refill-jobs"] tbody tr')
  expect(rows()).toHaveLength(10)
  expect(rows()[0]!.text()).toContain('order-1')
  expect(w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.previous"]').attributes('disabled')).toBeDefined()
  await w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.next"]').trigger('click')
  expect(rows()).toHaveLength(10)
  expect(rows()[0]!.text()).toContain('order-11')
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect(rows()[0]!.text()).toContain('order-11')
  await w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.next"]').trigger('click')
  expect(rows()).toHaveLength(3)
  expect(rows()[0]!.text()).toContain('order-21')
  expect(w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.next"]').attributes('disabled')).toBeDefined()
  get.mockResolvedValue(responseWithJobs(12))
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect(rows()).toHaveLength(2)
  expect(rows()[0]!.text()).toContain('order-11')
  get.mockResolvedValue(responseWithJobs(0))
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect(rows()).toHaveLength(0)
  expect(w.find('[data-testid="refill-jobs-pagination"]').exists()).toBe(false)
  expect(w.text()).toContain('accountRefill.noJobs')
  expect(manual).not.toHaveBeenCalled()
  w.unmount()
 })
 it('returns to the first page when switching groups or reopening', async () => {
  get.mockResolvedValue(responseWithJobs(23))
  const w = await open()
  const progress = async () => { await w.findAll('nav button').find(b => b.text() === 'accountRefill.progress')!.trigger('click') }
  await progress()
  await w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.next"]').trigger('click')
  get.mockResolvedValue({ ...responseWithJobs(23), config: { ...structuredClone(config), group_id: 3, delivery_group_ids: [3] } })
  await w.get('[data-testid="refill-rule-group"]').setValue(3); await flushPromises()
  expect(w.get('[data-testid="refill-jobs-pagination"] [aria-current="page"]').text()).toBe('1')
  await w.get('[data-testid="refill-jobs-pagination"] [aria-label="pagination.next"]').trigger('click')
  await w.setProps({ show: false }); await w.setProps({ show: true }); await flushPromises()
  await progress()
  expect(w.get('[data-testid="refill-jobs-pagination"] [aria-current="page"]').text()).toBe('1')
  w.unmount()
 })
 it('loads independent group rules and scopes subsequent polling and purchases', async () => {
  const w = await open()
  get.mockImplementation(async (id?: number) => ({ ...response(), config: { ...structuredClone(config), group_id: id || 2, delivery_group_ids: [id || 2], min_ready: id === 3 ? 4 : 1 } }))
  await w.get('[data-testid="refill-rule-group"]').setValue(3); await flushPromises()
  expect(get).toHaveBeenLastCalledWith(3)
  expect((w.get('[data-testid="refill-min_ready"]').element as HTMLInputElement).value).toBe('4')
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect(get).toHaveBeenLastCalledWith(3)
  await w.get('[data-testid="refill-min_ready"]').setValue(6)
  expect(w.get('[data-testid="refill-rule-group"]').attributes('disabled')).toBeDefined()
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ group_id: 3, min_ready: 6, delivery_group_ids: [3] }), '', 1, '')
  w.unmount()
 })
 it('saves independent plan quota thresholds and restores the general fallback', async () => {
  const w = await open()
  await w.get('[data-testid="refill-quota-pro5x-enabled"]').setValue(true)
  await w.get('[data-testid="refill-quota-pro20x-enabled"]').setValue(true)
  await w.get('[data-testid="refill-quota-pro5x-5h"]').setValue(15)
  await w.get('[data-testid="refill-quota-pro5x-7d"]').setValue(12.5)
  await w.get('[data-testid="refill-quota-pro20x-5h"]').setValue(10)
  await w.get('[data-testid="refill-quota-pro20x-7d"]').setValue(9)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ plan_quota_thresholds: { pro5x: { min_remaining_5h: 15, min_remaining_7d: 12.5 }, pro20x: { min_remaining_5h: 10, min_remaining_7d: 9 } }, min_remaining_5h: 15, min_remaining_7d: 15 }), '', 1, '')
  await w.get('[data-testid="refill-quota-pro5x-enabled"]').setValue(false)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ plan_quota_thresholds: { pro20x: { min_remaining_5h: 10, min_remaining_7d: 9 } } }), '', 2, '')
  w.unmount()
 })
 it('saves an ordered custom rotation without changing the fixed plan', async () => {
  const w = await open()
  await w.get('[data-testid="refill-rotation-enabled"]').setValue(true)
  expect(w.findAll('[data-testid="refill-rotation-step"]')).toHaveLength(2)
  await w.get('[data-testid="refill-rotation-count-1"]').setValue(3)
  expect(w.get('[data-testid="refill-rotation-preview"]').text()).toContain('Pro20x × 1 → Pro5x × 3')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ rotation_enabled: true, plan: 'pro5x', rotation: [{ plan: 'pro20x', count: 1 }, { plan: 'pro5x', count: 3 }] }), '', 1, '')
  expect(manual).not.toHaveBeenCalled()
  w.unmount()
 })
 it('discards a stale poll from the previous group', async () => {
  const w = await open()
  let resolvePoll!: (value: ReturnType<typeof response>) => void
  get.mockImplementationOnce(() => new Promise(resolve => { resolvePoll = resolve }))
  await vi.advanceTimersByTimeAsync(15000)
  get.mockResolvedValueOnce({ ...response(), config: { ...structuredClone(config), group_id: 3, delivery_group_ids: [3], min_ready: 8 } })
  await w.get('[data-testid="refill-rule-group"]').setValue(3); await flushPromises()
  resolvePoll(response()); await flushPromises()
  expect((w.get('[data-testid="refill-rule-group"]').element as HTMLSelectElement).value).toBe('3')
  expect((w.get('[data-testid="refill-min_ready"]').element as HTMLInputElement).value).toBe('8')
  w.unmount()
 })
 it('selects a manual plan without changing saved rules and blocks dirty purchases', async () => {
  const w = await open()
  expect(manual).not.toHaveBeenCalled()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.manual')!.trigger('click')
  await w.get('[data-testid="manual-refill-plan"]').setValue('pro20x')
  await w.get('[data-testid="refill-import-grace"]').setValue(30)
  expect(w.get('[data-testid="manual-refill-submit"]').attributes('disabled')).toBeDefined()
  await w.get('[data-testid="refill-import-grace"]').setValue(60)
  await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
  expect(manual).toHaveBeenCalledWith({ request_id: expect.stringMatching(/^\d{13}:/), plan: 'pro20x', revision: 1, group_id: 2 })
  expect(save).not.toHaveBeenCalled()
  expect(sessionStorage.getItem('yoshub:manual-refill:pending')).toBeNull()
  w.unmount()
 })
 it('keeps the same purchase request after timeout and reopening', async () => {
  manual.mockRejectedValueOnce(new Error('timeout'))
  let w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.manual')!.trigger('click')
  await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
  const first = manual.mock.calls[0][0]
  w.unmount(); w = await open()
  await w.findAll('nav button').find(b => b.text() === 'accountRefill.manual')!.trigger('click')
  expect(w.get('[data-testid="manual-refill-plan"]').attributes('disabled')).toBeDefined()
  await w.get('[data-testid="manual-refill-submit"]').trigger('click'); await flushPromises()
  expect(manual).toHaveBeenLastCalledWith(first)
  w.unmount()
 })
 it('saves independent notification switches and clears the secret field', async () => {
  const w = await open()
  await w.get('[data-testid="refill-notify-start"]').setValue(true)
  await w.get('[data-testid="refill-notify-balance"]').setValue(true)
  await w.get('[data-testid="refill-balance-threshold"]').setValue(123.5)
  await w.get('[data-testid="refill-webhook"]').setValue('https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=fixture')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ notifications: { refill_started: true, low_balance: true, low_balance_threshold: 123.5 } }), '', 1, expect.stringContaining('key=fixture'))
  expect((w.get('[data-testid="refill-webhook"]').element as HTMLInputElement).value).toBe('')
  w.unmount()
 })
 it('refreshes local evaluation every 15 seconds even while a provider check is pending', async () => {
  let finish!: (value: ReturnType<typeof response>) => void
  check.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
  const w = await open()
  await w.findAll('button').find(b => b.text() === 'accountRefill.check')!.trigger('click')
  const calls = get.mock.calls.length
  await vi.advanceTimersByTimeAsync(30000); await flushPromises()
  expect(get).toHaveBeenCalledTimes(calls + 2)
  expect(check).toHaveBeenCalledTimes(1)
  expect(manual).not.toHaveBeenCalled()
  finish(response()); await flushPromises(); w.unmount()
 })
 it('saves custom observation and import retention without changing automation switches', async () => {
  const w = await open()
  await w.get('[data-testid="refill-concurrency_sustain_seconds"]').setValue(90)
  await w.get('[data-testid="refill-import-grace"]').setValue(0)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ concurrency_sustain_seconds: 90, import_expiry_grace_minutes: 0, enabled: false, monitor_enabled: true }), '', 1, '')
  w.unmount()
 })
 it('shows wallet and key metadata without returning a secret, and does not purchase on open', async () => {
  const w = await open(); expect(w.text()).toContain('4,825.5'); expect(w.text()).toContain('4,765.5'); expect(w.text()).toContain('key label'); expect((w.get('[data-testid="refill-token"]').element as HTMLInputElement).value).toBe(''); expect(save).not.toHaveBeenCalled(); expect(check).not.toHaveBeenCalled(); w.unmount()
 })
 it('keeps edits during polling, saves a new key once, then clears the input', async () => {
  const w = await open(); await w.get('[data-testid="refill-token"]').setValue('rst_new_key'); await w.get('[data-testid="refill-enabled"]').setValue(true)
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect((w.get('[data-testid="refill-enabled"]').element as HTMLInputElement).checked).toBe(true)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ enabled: true }), 'rst_new_key', 1, '')
  expect((w.get('[data-testid="refill-token"]').element as HTMLInputElement).value).toBe('')
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined(); w.unmount()
 })
 it('ignores a poll started before a successful save', async () => {
  const w = await open()
  let resolvePoll!: (value: ReturnType<typeof response>) => void
  get.mockImplementationOnce(() => new Promise(resolve => { resolvePoll = resolve }))
  await vi.advanceTimersByTimeAsync(15000)
  await w.get('[data-testid="refill-enabled"]').setValue(true)
  await w.get('form').trigger('submit'); await flushPromises()
  resolvePoll(response(1)); await flushPromises()
  expect((w.get('[data-testid="refill-enabled"]').element as HTMLInputElement).checked).toBe(true)
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined()
  w.unmount()
 })
 it('blocks a stale form and stops polling after close' , async () => {
  const w = await open(); await w.get('[data-testid="refill-enabled"]').setValue(true); get.mockResolvedValue(response(2))
  await vi.advanceTimersByTimeAsync(15000); await flushPromises(); expect(w.text()).toContain('accountRefill.conflict')
  await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled()
  await w.setProps({ show: false }); const count = get.mock.calls.length; await vi.advanceTimersByTimeAsync(30000); expect(get).toHaveBeenCalledTimes(count); w.unmount()
 })
})
