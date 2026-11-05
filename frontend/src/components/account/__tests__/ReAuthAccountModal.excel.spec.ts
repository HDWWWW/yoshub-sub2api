import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'
import { OPENAI_EXCEL_CLIENT_ID } from '@/composables/useOpenAIOAuth'
import ReAuthAccountModal from '../ReAuthAccountModal.vue'

vi.mock('@/api/admin', () => ({ adminAPI: { accounts: {
  generateAuthUrl: vi.fn(), exchangeCode: vi.fn(), update: vi.fn(), clearError: vi.fn()
} } }))

const Flow = defineComponent({
  emits: ['generate-url'],
  data: () => ({ authCode: 'synthetic-code', oauthState: 'test-state', inputMethod: 'manual' }),
  methods: { reset() {} },
  template: '<button data-testid="generate" @click="$emit(\'generate-url\')">Generate</button>'
})

beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(adminAPI.accounts.generateAuthUrl).mockResolvedValue({
    auth_url: 'https://auth.example.com/?state=test-state', session_id: 'test-session'
  })
  vi.mocked(adminAPI.accounts.exchangeCode).mockResolvedValue({
    access_token: 'synthetic-token', refresh_token: 'synthetic-refresh', client_id: OPENAI_EXCEL_CLIENT_ID
  })
})

function create(clientId: string, enabled: boolean) {
  return mount(ReAuthAccountModal, {
    props: { show: true, account: {
      id: 123, name: 'test-account', platform: 'openai', type: 'oauth', proxy_id: 17,
      credentials: { client_id: clientId, model_mapping: { alias: 'gpt-6-astra' } },
      extra: { openai_excel_bps: enabled, openai_excel_bps_models: ['gpt-6-astra'], openai_excel_bps_omit_unsupported_tools: true }
    } as unknown as Account },
    global: {
      plugins: [createPinia(), createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })],
      stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Icon: true, OAuthAuthorizationFlow: Flow }
    }
  })
}

describe('Excel account reauthorization', () => {
  it.each([true, false])('uses Excel grant and keeps route enabled=%s after token rotation', async enabled => {
    const wrapper = create(OPENAI_EXCEL_CLIENT_ID, enabled)
    await wrapper.get('[data-testid="generate"]').trigger('click')
    await flushPromises()
    expect(adminAPI.accounts.generateAuthUrl).toHaveBeenCalledWith('/admin/openai/generate-auth-url', { proxy_id: 17, oauth_profile: 'excel' })
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(adminAPI.accounts.update).toHaveBeenCalledWith(123, expect.objectContaining({
      credentials: expect.objectContaining({ client_id: OPENAI_EXCEL_CLIENT_ID, model_mapping: { alias: 'gpt-6-astra' } }),
      extra: expect.objectContaining({ openai_excel_bps: enabled, openai_excel_bps_models: ['gpt-6-astra'], openai_excel_bps_omit_unsupported_tools: true })
    }))
    expect(wrapper.emitted('reauthorized')).toHaveLength(1)
    wrapper.unmount()
  })

  it('retains the original login request for Codex accounts', async () => {
    const wrapper = create('app_EMoamEEZ73f0CkXaXp7hrann', false)
    await wrapper.get('[data-testid="generate"]').trigger('click')
    await flushPromises()
    expect(adminAPI.accounts.generateAuthUrl).toHaveBeenCalledWith('/admin/openai/generate-auth-url', { proxy_id: 17 })
    wrapper.unmount()
  })
})
