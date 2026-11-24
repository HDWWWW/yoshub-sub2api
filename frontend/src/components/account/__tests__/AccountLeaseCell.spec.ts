import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import AccountLeaseCell from '../AccountLeaseCell.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('supplier and local lease deadlines', () => {
  it('renders the full Beijing deadline across midnight and keeps a pending supplier date separate', async () => {
    const metadata = {
      account_id: 1, source_expires_at: '2026-11-24T16:15:00Z', source_expiry_origin: 'supplier_api',
      effective_expires_at: '2026-11-24T16:45:00Z', grace_minutes: 30, manual_priority: null
    }
    const wrapper = mount(AccountLeaseCell, { props: { metadata, autoPause: true, failed: false } })
    expect(wrapper.text()).toContain('2026-11-25 00:15:00')
    expect(wrapper.text()).toContain('2026-11-25 00:45:00')
    await wrapper.setProps({ metadata: { ...metadata, source_expires_at: null, source_expiry_origin: 'supplier_pending' } })
    expect(wrapper.text()).toContain('accountRefill.supplierExpiryPending')
    expect(wrapper.text()).toContain('2026-11-25 00:45:00')
  })
})
