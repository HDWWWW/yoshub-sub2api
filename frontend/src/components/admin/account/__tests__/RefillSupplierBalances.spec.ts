import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import RefillSupplierBalances from '../RefillSupplierBalances.vue'
import type { RefillView } from '@/api/admin/accountRefill'
import zh from '@/i18n/locales/zh/accountRefill'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => zh[key.replace('accountRefill.', '') as keyof typeof zh] || key }) }))

type BalanceView = Pick<RefillView, 'token_configured' | 'wallet' | 'catalog' | 'identity_at' | 'catalog_at' | 'supplier_558669' | 'muze'>
function fixture(): BalanceView {
  return {
    token_configured: true, wallet: { balance: 5109, held: 0, available: 5109 }, catalog: null,
    identity_at: '2026-11-25T02:40:24Z', catalog_at: null,
    supplier_558669: { token_configured: true, checked_at: '2026-11-25T02:41:10Z', retry_at: '', daily: { count: 0, points: 0, reserved_points: 0, spent_points: 0 }, catalog: { available: 90, reserved: 10, items: [] } },
    muze: { token_configured: true, checked_at: '2026-11-25T02:42:00Z', connection_id: '', products: [
      { product: 'pro20x', label: 'Pro20x', plan: 'pro20x', mode: '', closed: false, expired: false, stock: 3, remaining: 8, available_now: 3, blocked_reason: '' },
      { product: 'pro20x_v2', label: 'Pro20x V2', plan: 'pro20x', mode: '', closed: false, expired: false, stock: 5, remaining: 8, available_now: 5, blocked_reason: '' }
    ] }
  }
}

describe('Configured refill supplier balances', () => {
  it('shows all configured suppliers in their native units without aggregating Muze variants', () => {
    const w = mount(RefillSupplierBalances, { props: { view: fixture() } })
    expect(w.get('[data-testid="refill-wallet-1208"]').findAll('dd').map(e => e.text())).toEqual(['5,109', '0', '5,109'])
    expect(w.get('[data-testid="refill-wallet-558669"]').findAll('dd').map(e => e.text())).toEqual(['100', '10', '90'])
    const products = w.get('[data-testid="refill-wallet-muze"]').findAll('[data-testid="refill-muze-product"]')
    expect(products).toHaveLength(2)
    expect(products[0]!.findAll('dd').map(e => e.text())).toEqual(['8', '3', '3'])
    expect(products[1]!.findAll('dd').map(e => e.text())).toEqual(['8', '5', '5'])
    expect(w.get('[data-testid="refill-wallet-1208"]').text()).toContain('10:40:24')
    expect(w.get('[data-testid="refill-wallet-558669"]').text()).toContain('10:41:10')
  })

  it('hides unconfigured suppliers even when stale balance snapshots exist', () => {
    const view = fixture()
    view.token_configured = false
    view.muze!.token_configured = false
    const w = mount(RefillSupplierBalances, { props: { view } })
    expect(w.find('[data-testid="refill-wallet-1208"]').exists()).toBe(false)
    expect(w.find('[data-testid="refill-wallet-muze"]').exists()).toBe(false)
    expect(w.find('[data-testid="refill-wallet-558669"]').exists()).toBe(true)
  })

  it('does not turn missing snapshots into a zero balance', () => {
    const view = fixture()
    view.wallet = null
    view.supplier_558669!.catalog = null
    view.muze!.products = null
    const w = mount(RefillSupplierBalances, { props: { view } })
    expect(w.get('[data-testid="refill-wallet-1208"]').findAll('dd').map(e => e.text())).toEqual(['—', '—', '—'])
    expect(w.get('[data-testid="refill-wallet-558669"]').findAll('dd').map(e => e.text())).toEqual(['—', '—', '—'])
    expect(w.get('[data-testid="refill-wallet-muze"]').text()).toContain(zh.muzeCatalogEmpty)
  })

  it('updates only the corresponding supplier after a refreshed view arrives', async () => {
    const view = fixture()
    const w = mount(RefillSupplierBalances, { props: { view } })
    const updated = structuredClone(view)
    updated.supplier_558669!.catalog!.available = 45
    updated.muze!.products![0]!.expired = true
    await w.setProps({ view: updated })
    expect(w.get('[data-testid="refill-wallet-558669"]').findAll('dd').map(e => e.text())).toEqual(['55', '10', '45'])
    expect(w.get('[data-testid="refill-wallet-1208"]').findAll('dd').map(e => e.text())).toEqual(['5,109', '0', '5,109'])
    expect(w.get('[data-testid="refill-wallet-muze"]').text()).toContain(zh.expired)
  })

  it('renders no summary until at least one supplier is configured', () => {
    const view = fixture()
    view.token_configured = false
    view.supplier_558669!.token_configured = false
    view.muze!.token_configured = false
    const w = mount(RefillSupplierBalances, { props: { view } })
    expect(w.find('[data-testid="refill-supplier-balances"]').exists()).toBe(false)
  })
})
