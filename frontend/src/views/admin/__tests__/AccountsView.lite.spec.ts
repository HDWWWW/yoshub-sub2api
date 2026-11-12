import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DOMWrapper, flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import AccountTableActions from '@/components/admin/account/AccountTableActions.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  refreshCredentials,
  setForceFast,
  showError,
  showWarning
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  refreshCredentials: vi.fn(),
  setForceFast: vi.fn(),
  showError: vi.fn(),
  showWarning: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      setForceFast,
      refreshCredentials
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showWarning, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/api/admin/accountScheduling', () => ({
  accountSchedulingAPI: { metadata: vi.fn().mockResolvedValue([]) }
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] }, columns: { type: Array, default: () => [] }, loading: Boolean },
  template: `
    <div>
      <template v-if="!loading"><div v-for="row in data" :key="row.id" :data-account-name="row.name">
        <slot v-if="columns.some(column => column.key === 'force_fast')" name="cell-force_fast" :row="row" />
        <slot name="cell-groups" :row="row" />
        <slot name="cell-actions" :row="row" />
      </div></template>
    </div>
  `
})

const AccountGroupsCellStub = defineComponent({
  props: { groups: { type: Array, default: () => [] } },
  template: '<span data-test="account-groups">{{ groups.map(group => group.name).join(",") }}</span>'
})

const EditAccountModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="edit-account">{{ show ? account?.name : "" }}</div>'
})

const AccountTestModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="test-account">{{ show ? account?.name : "" }}</div>'
})

const AccountStatsModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="stats-account">{{ show ? account?.name : "" }}</div>'
})

function mountView(stubActionMenu = true) {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: `<div><button data-test="manual-refresh" @click="$emit('refresh')">refresh</button><slot name="after" /></div>` },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: stubActionMenu,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: AccountTestModalStub,
        AccountStatsModal: AccountStatsModalStub,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: EditAccountModalStub,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: AccountGroupsCellStub,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: stubActionMenu
      }
    }
  })
}

const listRow = {
  id: 42,
  name: 'compact row',
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [7],
  extra: {},
  credentials: {}
}

const fullAccount = {
  ...listRow,
  groups: [{ id: 7, name: 'codex', platform: 'openai' }],
  account_groups: [{ account_id: 42, group_id: 7 }],
  credentials: { api_key: 'redacted' },
  extra: { detail_only: true }
}

describe('admin AccountsView lite account list', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'compact-etag', data: null })
    getById.mockReset().mockResolvedValue(fullAccount)
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'codex', platform: 'openai' }])
    refreshCredentials.mockReset()
    setForceFast.mockReset()
    showError.mockReset()
    showWarning.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('keeps lite=1 on the initial list request', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it('keeps existing rows mounted and busy while a manual list refresh is pending', async () => {
    const wrapper = mountView()
    await flushPromises()
    const row = wrapper.get('[data-account-name="compact row"]').element
    let finish: (value: unknown) => void = () => {}
    listAccounts.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await wrapper.get('[data-test="manual-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.getComponent(DataTableStub).props('loading')).toBe(false)
    expect(wrapper.get('[data-account-name="compact row"]').element).toBe(row)
    expect(wrapper.find('[aria-busy="true"]').attributes()).toHaveProperty('inert')
    finish({ items: [{ ...listRow, name: 'updated row' }], total: 1, page: 1, page_size: 20, pages: 1 })
    await flushPromises()
    expect(wrapper.get('[data-account-name="updated row"]').element).toBe(row)
    expect(wrapper.find('[aria-busy="true"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('still shows initial loading when no rows are available', async () => {
    let finish: (value: unknown) => void = () => {}
    listAccounts.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.getComponent(DataTableStub).props('loading')).toBe(true)
    finish({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
    await flushPromises()
    wrapper.unmount()
  })

  it('maps group_ids through the group catalog for the table cell', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="account-groups"]').text()).toBe('codex')
    wrapper.unmount()
  })

  it('shows Force Fast beside scheduling and saves both toggle directions without loading account details', async () => {
    setForceFast.mockResolvedValueOnce({ enabled: true }).mockResolvedValueOnce({ enabled: false })
    const wrapper = mountView()
    await flushPromises()

    const table = wrapper.getComponent(DataTableStub)
    const columns = table.props('columns') as Array<{ key: string }>
    expect(columns[columns.findIndex(column => column.key === 'schedulable') + 1].key).toBe('force_fast')
    const toggle = wrapper.get('[role="switch"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    await toggle.trigger('click')
    await flushPromises()
    expect(setForceFast).toHaveBeenLastCalledWith(42, true)
    expect(toggle.attributes('aria-checked')).toBe('true')
    await toggle.trigger('click')
    await flushPromises()
    expect(setForceFast).toHaveBeenLastCalledWith(42, false)
    expect(toggle.attributes('aria-checked')).toBe('false')
    expect(getById).not.toHaveBeenCalled()
    expect((table.props('data') as Array<typeof listRow>)[0].schedulable).toBe(true)
    wrapper.unmount()
  })

  it('restores Force Fast from the lite list and hides the switch for other platforms', async () => {
    listAccounts.mockResolvedValue({
      items: [
        { ...listRow, extra: { openai_force_fast: true } },
        { ...listRow, id: 43, name: 'Claude', platform: 'anthropic' }
      ], total: 2, page: 1, page_size: 20, pages: 1
    })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.findAll('[role="switch"]')).toHaveLength(1)
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  it('keeps saved state on failure and allows retrying Force Fast', async () => {
    setForceFast.mockRejectedValueOnce(new Error('save failed')).mockResolvedValueOnce({ enabled: true })
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const wrapper = mountView()
    await flushPromises()
    const toggle = wrapper.get('[role="switch"]')
    await toggle.trigger('click')
    await flushPromises()
    expect(toggle.attributes('aria-checked')).toBe('false')
    expect(toggle.attributes('disabled')).toBeUndefined()
    expect(showError).toHaveBeenCalledWith('admin.accounts.failedToToggleForceFast')
    await toggle.trigger('click')
    await flushPromises()
    expect(toggle.attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  it('tracks pending Force Fast saves per account and ignores repeat clicks', async () => {
    listAccounts.mockResolvedValue({
      items: [listRow, { ...listRow, id: 43, name: 'Second' }], total: 2, page: 1, page_size: 20, pages: 1
    })
    let finishFirst!: (value: { enabled: boolean }) => void
    let finishSecond!: (value: { enabled: boolean }) => void
    setForceFast.mockImplementationOnce(() => new Promise(resolve => { finishFirst = resolve }))
      .mockImplementationOnce(() => new Promise(resolve => { finishSecond = resolve }))
    const wrapper = mountView()
    await flushPromises()
    const [first, second] = wrapper.findAll('[role="switch"]')
    await first.trigger('click')
    await first.trigger('click')
    expect(setForceFast).toHaveBeenCalledTimes(1)
    expect(first.attributes('disabled')).toBeDefined()
    await second.trigger('click')
    expect(setForceFast).toHaveBeenCalledTimes(2)
    finishFirst({ enabled: true })
    await flushPromises()
    expect(first.attributes('disabled')).toBeUndefined()
    expect(second.attributes('disabled')).toBeDefined()
    finishSecond({ enabled: true })
    await flushPromises()
    expect(second.attributes('disabled')).toBeUndefined()
    expect(first.attributes('aria-checked')).toBe('true')
    expect(second.attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  it.each(['manual', 'automatic'])('keeps a confirmed Fast toggle when an older %s refresh arrives late', async mode => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    if (mode === 'automatic') localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    setForceFast.mockResolvedValue({ enabled: true })
    const wrapper = mountView()
    await flushPromises()
    let finishRefresh!: (value: unknown) => void
    const stale = { items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 }
    if (mode === 'manual') {
      listAccounts.mockImplementationOnce(() => new Promise(resolve => { finishRefresh = resolve }))
      wrapper.findComponent(AccountTableActions).vm.$emit('refresh')
    } else {
      listWithEtag.mockImplementationOnce(() => new Promise(resolve => { finishRefresh = resolve }))
      await vi.advanceTimersByTimeAsync(6000)
    }
    await flushPromises()
    expect(finishRefresh).toBeTypeOf('function')
    await wrapper.get('[role="switch"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    finishRefresh(mode === 'manual' ? stale : { notModified: false, etag: 'stale-etag', data: stale })
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    // A new read started after the save must still observe changes from elsewhere.
    wrapper.findComponent(AccountTableActions).vm.$emit('refresh')
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })

  it('keeps the action menu open during internal scrolling but closes it on table scrolling', async () => {
    const wrapper = mountView(false)
    await flushPromises()

    const trigger = wrapper.findAll('button').find(button => button.text() === 'common.more')!
    await trigger.trigger('click')
    const menu = new DOMWrapper(document.body.querySelector('.action-menu-content')!)
    menu.element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    menu.get('button').element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    wrapper.getComponent(DataTableStub).element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(false)
    wrapper.unmount()
  })

  it('keeps lite=1 on automatic ETag refreshes', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const wrapper = mountView()
    await flushPromises()

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    expect(listWithEtag).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ etag: null })
    )
    wrapper.unmount()
  })

  it('loads the full account by id before opening edit, test, and stats actions', async () => {
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    expect(getById).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('compact row')

    const menu = wrapper.findComponent(AccountActionMenu)
    menu.vm.$emit('test', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="test-account"]').text()).toBe('compact row')

    menu.vm.$emit('stats', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(3)
    expect(wrapper.get('[data-test="stats-account"]').text()).toBe('compact row')
    wrapper.unmount()
  })

  it('shows the warning and patches the account after a partial Antigravity refresh', async () => {
    refreshCredentials.mockResolvedValue({
      account: { ...fullAccount, name: 'refreshed account' },
      message: 'Token refreshed, but project_id is temporarily unavailable',
      warning: 'missing_project_id_temporary'
    })
    const wrapper = mountView(false)
    await flushPromises()

    wrapper.findComponent(AccountActionMenu).vm.$emit('refresh-token', listRow)
    await flushPromises()

    expect(refreshCredentials).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-account-name]').attributes('data-account-name')).toBe('refreshed account')
    expect(showWarning).toHaveBeenCalledWith('Token refreshed, but project_id is temporarily unavailable')
    wrapper.unmount()
  })

  it('shows an error and keeps the modal closed when detail loading fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    getById.mockRejectedValueOnce(new Error('detail failed'))
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    await editButton!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('detail failed')
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('')
    consoleError.mockRestore()
    wrapper.unmount()
  })
})
