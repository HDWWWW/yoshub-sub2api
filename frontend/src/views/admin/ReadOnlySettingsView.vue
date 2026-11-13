<template>
  <AppLayout>
    <div class="mx-auto max-w-4xl space-y-5">
      <div class="flex items-center justify-between">
        <h1 class="text-xl font-semibold">{{ t('admin.settings.title') }}</h1>
        <button class="btn btn-secondary" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
      </div>
      <p v-if="error" role="alert" class="text-red-600">{{ error }}</p>
      <dl v-else class="divide-y divide-gray-100 rounded-xl border border-gray-200 bg-white px-5 dark:divide-dark-700 dark:border-dark-700 dark:bg-dark-800">
        <div v-for="field in fields" :key="field[0]" class="grid gap-2 py-4 sm:grid-cols-2">
          <dt class="text-sm text-gray-500">{{ field[locale.startsWith('zh') ? 1 : 2] }}</dt>
          <dd class="break-words text-sm">{{ display(settings[field[0]]) }}</dd>
        </div>
      </dl>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import AppLayout from '@/components/layout/AppLayout.vue'

const { t, locale } = useI18n()
const loading = ref(false)
const error = ref('')
const settings = ref<Record<string, unknown>>({})
const fields = [
  ['site_name', '站点名称', 'Site name'],
  ['site_subtitle', '站点副标题', 'Site subtitle'],
  ['api_base_url', 'API 端点地址', 'API base URL'],
  ['contact_info', '联系方式', 'Contact'],
  ['doc_url', '文档地址', 'Documentation'],
  ['registration_enabled', '开放注册', 'Registration'],
  ['email_verify_enabled', '邮箱验证', 'Email verification'],
  ['registration_email_suffix_whitelist', '注册邮箱域名', 'Registration email domains'],
  ['invite_only_enabled', '仅限邀请注册', 'Invitation required'],
  ['promo_code_enabled', '优惠码', 'Promo codes'],
  ['password_reset_enabled', '密码重置', 'Password reset'],
  ['default_balance', '默认余额', 'Default balance'],
  ['default_concurrency', '默认并发数', 'Default concurrency'],
  ['default_rpm_limit', '默认每分钟请求数', 'Default requests per minute'],
  ['ops_monitoring_enabled', '运维监控', 'Operations monitoring'],
  ['ops_realtime_monitoring_enabled', '实时监控', 'Realtime monitoring'],
  ['ops_query_mode_default', '监控查询模式', 'Monitoring query mode'],
  ['totp_enabled', '双因素认证', 'Two-factor authentication'],
  ['backend_mode_enabled', '纯后台模式', 'Backend mode'],
] as const

function display(value: unknown): string {
  if (typeof value === 'boolean') return value ? t('common.enabled') : t('common.disabled')
  if (Array.isArray(value)) return value.join(', ') || '—'
  return value == null || value === '' ? '—' : String(value)
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    settings.value = { ...await adminAPI.settings.getSettings() }
  } catch {
    error.value = t('common.error')
  } finally {
    loading.value = false
  }
}
onMounted(load)
</script>
