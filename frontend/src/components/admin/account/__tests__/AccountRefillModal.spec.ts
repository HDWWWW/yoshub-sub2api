import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRefillModal from '../AccountRefillModal.vue'

const { get, save, check, retry, manual } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn(), check: vi.fn(), retry: vi.fn(), manual: vi.fn() }))
vi.mock('@/api/admin/accountRefill', () => ({ accountRefillAPI: { get, save, check, retry, manual } }))
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
