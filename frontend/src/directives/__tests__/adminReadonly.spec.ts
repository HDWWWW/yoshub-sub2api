import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, nextTick, onMounted, reactive } from 'vue'
import { adminWrite } from '../adminWrite'
import { adminReadOnlyForm } from '../adminReadOnlyForm'
import { adminReadOnlyControl } from '../adminReadOnlyControl'
import AccountTableActions from '@/components/admin/account/AccountTableActions.vue'
import { isAdminMutation, readOnlyAdminPageAllowed } from '@/utils/adminPermissions'

const auth = reactive({ isReadOnlyAdmin: true })
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const directives = {
  'admin-write': adminWrite,
  'admin-read-only-form': adminReadOnlyForm,
  'admin-read-only-control': adminReadOnlyControl,
}

describe('ordinary administrator UI', () => {
  beforeEach(() => { auth.isReadOnlyAdmin = true })

  it('does not mount private account tools and keeps native refresh available', async () => {
    const mounted = vi.fn()
    const PrivateTool = defineComponent({ setup() { onMounted(mounted); return {} }, template: '<div>private refill control</div>' })
    const wrapper = mount(AccountTableActions, {
      props: { loading: false },
      slots: { before: PrivateTool },
      global: { directives },
    })
    expect(mounted).not.toHaveBeenCalled()
    expect(wrapper.text()).not.toContain('private refill control')
    expect(wrapper.text()).not.toContain('admin.accounts.createAccount')
    await wrapper.get('button').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    auth.isReadOnlyAdmin = false
    await nextTick()
    expect(mounted).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('admin.accounts.createAccount')
    wrapper.unmount()
  })

  it('hides mutations and prevents form submissions without disabling filters', async () => {
    const submit = vi.fn()
    const wrapper = mount(defineComponent({
      setup: () => ({ submit }),
      template: '<div><input data-filter /><form v-admin-read-only-form @submit="submit"><input data-config /><button v-admin-write type="submit">Save</button></form><button v-admin-read-only-control>Toggle</button></div>',
    }), { global: { directives }, attachTo: document.body })
    expect((wrapper.get('[data-filter]').element as HTMLInputElement).disabled).toBe(false)
    expect((wrapper.get('[data-config]').element as HTMLInputElement).disabled).toBe(true)
    expect(wrapper.get('button[type=submit]').isVisible()).toBe(false)
    await wrapper.get('form').trigger('submit')
    expect(submit).not.toHaveBeenCalled()
    auth.isReadOnlyAdmin = false
    await nextTick()
    expect((wrapper.get('[data-config]').element as HTMLInputElement).disabled).toBe(false)
    expect(wrapper.get('button[type=submit]').isVisible()).toBe(true)
    await wrapper.get('form').trigger('submit')
    expect(submit).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('blocks restricted navigation and writes but permits native statistics', () => {
    for (const path of ['/admin/happy-first-token', '/keys', '/settings/mcp', '/custom/owner-tool']) {
      expect(readOnlyAdminPageAllowed(path)).toBe(false)
    }
    for (const path of ['/admin/accounts', '/admin/groups', '/admin/users', '/admin/ops', '/admin/settings']) {
      expect(readOnlyAdminPageAllowed(path)).toBe(true)
    }
    expect(isAdminMutation('post', '/admin/accounts/today-stats/batch')).toBe(false)
    expect(isAdminMutation('post', '/api/v1/admin/accounts/1/test')).toBe(true)
    expect(isAdminMutation('put', '/admin/groups/2')).toBe(true)
    expect(isAdminMutation('get', '/admin/groups')).toBe(false)
  })
})
