import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRefillModal from '../AccountRefillModal.vue'

const { get, save, check, retry } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn(), check: vi.fn(), retry: vi.fn() }))
vi.mock('@/api/admin/accountRefill', () => ({ accountRefillAPI: { get, save, check, retry } }))
vi.mock('@/api/admin/groups', () => ({ getAll: async () => [{ id: 2, name: 'OpenAI', platform: 'openai', status: 'active' }] }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
const config = {
 enabled: false, monitor_enabled: true, mode: 'capacity', group_id: 2, delivery_group_ids: [2], push_enabled: true,
 max_in_flight: 1, interval_minutes: 60, interval_quantity: 1, plan: 'pro5x', min_ready: 1, max_ready: 20,
 lead_minutes: 30, min_remaining_5h: 15, min_remaining_7d: 15, target_concurrency_percent: 85,
 check_seconds: 60, cooldown_seconds: 300, daily_max_count: 50, daily_max_points: 3000, max_unit_price: 60,
 import_concurrency: 60, window_enabled: false, window_start: '09:00', window_end: '23:00',
 delivery: { ws_mode: 'http_bridge', proxy_mode: 'direct', proxy_id: null, name_template: '', notes_template: '', priority: 1, load_factor: 0, rate_multiplier: 1, status: 'active', schedulable: true, auto_pause_on_expired: true, fingerprint_mode: 'full', expiry_mode: 'rental', fixed_expires_at: null, model_mapping: {} }
}
function response(revision = 1) { return { config: structuredClone(config), revision, token_configured: true, key: { label: 'key label', scope: 'write', expires_at: '2027-01-29T12:59:37Z' }, wallet: { balance: 4825.5, held: 60, available: 4765.5 }, identity_at: '2026-10-31T12:00:00Z', catalog: { items: [{ service: 'refill', plan: 'pro5x', price: 60, enabled: true }] }, jobs: [], evaluation: { ready: 1, accounts: [] }, daily: {}, message: 'ready' } }
async function open() { const wrapper = mount(AccountRefillModal, { props: { show: true }, global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' } } } }); await flushPromises(); return wrapper }
describe('AccountRefillModal', () => {
 beforeEach(() => { vi.useFakeTimers(); vi.resetAllMocks(); get.mockResolvedValue(response()); save.mockImplementation(async (cfg) => ({ ...response(2), config: cfg })); check.mockResolvedValue(response()) })
 afterEach(() => vi.useRealTimers())
 it('shows wallet and key metadata without returning a secret, and does not purchase on open', async () => {
  const w = await open(); expect(w.text()).toContain('4,825.5'); expect(w.text()).toContain('4,765.5'); expect(w.text()).toContain('key label'); expect((w.get('[data-testid="refill-token"]').element as HTMLInputElement).value).toBe(''); expect(save).not.toHaveBeenCalled(); expect(check).not.toHaveBeenCalled(); w.unmount()
 })
 it('keeps edits during polling, saves a new key once, then clears the input', async () => {
  const w = await open(); await w.get('[data-testid="refill-token"]').setValue('rst_new_key'); await w.get('[data-testid="refill-enabled"]').setValue(true)
  await vi.advanceTimersByTimeAsync(15000); await flushPromises()
  expect((w.get('[data-testid="refill-enabled"]').element as HTMLInputElement).checked).toBe(true)
  await w.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(expect.objectContaining({ enabled: true }), 'rst_new_key', 1)
  expect((w.get('[data-testid="refill-token"]').element as HTMLInputElement).value).toBe('')
  expect(w.get('[data-testid="refill-save"]').attributes('disabled')).toBeDefined(); w.unmount()
 })
 it('blocks a stale form and stops polling after close', async () => {
  const w = await open(); await w.get('[data-testid="refill-enabled"]').setValue(true); get.mockResolvedValue(response(2))
  await vi.advanceTimersByTimeAsync(15000); await flushPromises(); expect(w.text()).toContain('accountRefill.conflict')
  await w.get('form').trigger('submit'); expect(save).not.toHaveBeenCalled()
  await w.setProps({ show: false }); const count = get.mock.calls.length; await vi.advanceTimersByTimeAsync(30000); expect(get).toHaveBeenCalledTimes(count); w.unmount()
 })
})
