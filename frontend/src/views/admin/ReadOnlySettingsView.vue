<template>
  <AppLayout>
    <div class="mx-auto max-w-5xl space-y-5">
      <div class="flex items-center justify-between">
        <h1 class="text-xl font-semibold">{{ t('admin.settings.title') }}</h1>
        <button class="btn btn-secondary" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
      </div>
      <nav class="flex flex-wrap gap-2" :aria-label="t('admin.settings.title')">
        <button v-for="tab in tabs" :key="tab" class="btn" :class="activeTab === tab ? 'btn-primary' : 'btn-secondary'" :aria-current="activeTab === tab ? 'page' : undefined" @click="activeTab = tab">
          {{ t(`admin.settings.tabs.${tab}`) }}
        </button>
      </nav>
      <BackupSettings v-if="activeTab === 'backup'" />
      <p v-else-if="error" role="alert" class="text-red-600">{{ error }}</p>
      <dl v-else class="divide-y divide-gray-100 rounded-xl border border-gray-200 bg-white px-5 dark:divide-dark-700 dark:border-dark-700 dark:bg-dark-800">
        <div v-for="field in fields[activeTab]" :key="field[0]" class="grid gap-2 py-4 sm:grid-cols-2">
          <dt class="text-sm text-gray-500">{{ field[locale.startsWith('zh') ? 1 : 2] }}</dt>
          <dd class="whitespace-pre-wrap break-words text-sm">{{ display(settings[field[0]]) }}</dd>
        </div>
      </dl>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { defineAsyncComponent, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import AppLayout from '@/components/layout/AppLayout.vue'

const BackupSettings = defineAsyncComponent(() => import('./BackupView.vue'))
const { t, locale } = useI18n()
const loading = ref(false)
const error = ref('')
const settings = ref<Record<string, unknown>>({})
const tabs = ['general', 'agreement', 'features', 'security', 'users', 'gateway', 'payment', 'email', 'backup'] as const
type Tab = typeof tabs[number]
const activeTab = ref<Tab>('general')
const fields: Record<Exclude<Tab, 'backup'>, readonly (readonly [string, string, string])[]> = {
  general: [
  ['site_name', '站点名称', 'Site name'],
  ['site_subtitle', '站点副标题', 'Site subtitle'],
  ['api_base_url', 'API 端点地址', 'API base URL'],
  ['frontend_url', '前端地址', 'Frontend URL'],
  ['contact_info', '联系方式', 'Contact'],
  ['doc_url', '文档地址', 'Documentation'],
  ['compact_home_enabled', '简洁首页', 'Compact home'],
  ['table_default_page_size', '表格默认每页条数', 'Default page size'],
  ['table_page_size_options', '表格每页条数选项', 'Page size options'],
  ],
  agreement: [
    ['login_agreement_enabled', '登录协议', 'Login agreement'],
    ['login_agreement_mode', '协议确认方式', 'Agreement mode'],
    ['login_agreement_updated_at', '协议更新时间', 'Agreement updated at'],
    ['login_agreement_documents', '协议文档', 'Agreement documents'],
  ],
  features: [
    ['backend_mode_enabled', '纯后台模式', 'Backend mode'],
    ['subscription_enabled', '订阅功能', 'Subscriptions'],
    ['purchase_subscription_enabled', '购买订阅', 'Subscription purchase'],
    ['purchase_subscription_url', '购买订阅地址', 'Subscription purchase URL'],
    ['available_channels_enabled', '可用渠道', 'Available channels'],
    ['channel_monitor_enabled', '渠道监控', 'Channel monitoring'],
    ['channel_monitor_mode', '渠道监控模式', 'Channel monitoring mode'],
    ['channel_monitor_default_interval_seconds', '渠道监控间隔（秒）', 'Monitor interval (seconds)'],
    ['model_plaza_enabled', '模型广场', 'Model plaza'],
    ['model_plaza_require_auth', '模型广场需登录', 'Model plaza requires login'],
    ['plugin_management_enabled', '插件管理', 'Plugin management'],
    ['risk_control_enabled', '风控中心', 'Risk control'],
    ['affiliate_enabled', '邀请返利', 'Affiliate rebates'],
    ['ops_monitoring_enabled', '运维监控', 'Operations monitoring'],
    ['ops_realtime_monitoring_enabled', '实时监控', 'Realtime monitoring'],
    ['ops_query_mode_default', '监控查询模式', 'Monitoring query mode'],
  ],
  security: [
    ['totp_enabled', '双因素认证', 'Two-factor authentication'],
    ['passkey_enabled', '通行密钥登录', 'Passkey login'],
    ['session_binding_enabled', '会话绑定', 'Session binding'],
    ['step_up_enabled', '敏感操作二次认证', 'Sensitive operation verification'],
    ['audit_log_retention_days', '操作日志保留天数', 'Audit retention days'],
    ['turnstile_enabled', 'Turnstile 验证', 'Turnstile verification'],
    ['tencent_captcha_enabled', '腾讯验证码', 'Tencent CAPTCHA'],
    ['aliyun_captcha_enabled', '阿里云验证码', 'Aliyun CAPTCHA'],
    ['linuxdo_connect_enabled', 'Linux DO 登录', 'Linux DO login'],
    ['dingtalk_connect_enabled', '钉钉登录', 'DingTalk login'],
    ['wechat_connect_enabled', '微信登录', 'WeChat login'],
    ['oidc_connect_enabled', 'OIDC 登录', 'OIDC login'],
    ['github_oauth_enabled', 'GitHub 登录', 'GitHub login'],
    ['google_oauth_enabled', 'Google 登录', 'Google login'],
  ],
  users: [
  ['registration_enabled', '开放注册', 'Registration'],
  ['email_verify_enabled', '邮箱验证', 'Email verification'],
  ['registration_email_suffix_whitelist', '注册邮箱域名', 'Registration email domains'],
  ['invitation_code_enabled', '邀请码注册', 'Invitation codes'],
  ['promo_code_enabled', '优惠码', 'Promo codes'],
  ['password_reset_enabled', '密码重置', 'Password reset'],
  ['default_balance', '默认余额', 'Default balance'],
  ['default_concurrency', '默认并发数', 'Default concurrency'],
  ['default_user_rpm_limit', '默认每分钟请求数', 'Default requests per minute'],
  ['default_subscriptions', '默认订阅', 'Default subscriptions'],
  ['default_platform_quotas', '默认平台配额', 'Default platform quotas'],
  ],
  gateway: [
    ['enable_model_fallback', '模型回退', 'Model fallback'],
    ['fallback_model_anthropic', 'Anthropic 回退模型', 'Anthropic fallback model'],
    ['fallback_model_openai', 'OpenAI 回退模型', 'OpenAI fallback model'],
    ['fallback_model_gemini', 'Gemini 回退模型', 'Gemini fallback model'],
    ['fallback_model_antigravity', 'Antigravity 回退模型', 'Antigravity fallback model'],
    ['enable_fingerprint_unification', '指纹统一', 'Fingerprint unification'],
    ['enable_metadata_passthrough', '元数据透传', 'Metadata passthrough'],
    ['enable_identity_patch', '身份补丁', 'Identity patch'],
    ['min_claude_code_version', 'Claude Code 最低版本', 'Minimum Claude Code version'],
    ['max_claude_code_version', 'Claude Code 最高版本', 'Maximum Claude Code version'],
    ['web_search_emulation_enabled', '搜索模拟', 'Web search emulation'],
  ],
  payment: [
    ['payment_enabled', '支付功能', 'Payments'],
    ['payment_min_amount', '最低支付金额', 'Minimum payment'],
    ['payment_max_amount', '最高支付金额', 'Maximum payment'],
    ['payment_daily_limit', '每日支付限额', 'Daily payment limit'],
    ['payment_order_timeout_minutes', '订单超时（分钟）', 'Order timeout (minutes)'],
    ['payment_max_pending_orders', '最大待支付订单数', 'Maximum pending orders'],
    ['payment_enabled_types', '支付方式', 'Payment methods'],
    ['payment_balance_disabled', '禁用余额充值', 'Balance recharge disabled'],
    ['payment_balance_recharge_multiplier', '余额充值倍率', 'Balance recharge multiplier'],
    ['payment_subscription_usd_to_cny_rate', '订阅美元兑人民币汇率', 'Subscription USD/CNY rate'],
    ['payment_recharge_fee_rate', '充值手续费率', 'Recharge fee rate'],
    ['payment_load_balance_strategy', '支付负载策略', 'Payment load balancing'],
  ],
  email: [
    ['smtp_host', 'SMTP 服务器', 'SMTP host'],
    ['smtp_port', 'SMTP 端口', 'SMTP port'],
    ['smtp_username', 'SMTP 用户名', 'SMTP username'],
    ['smtp_from_email', '发件邮箱', 'Sender email'],
    ['smtp_from_name', '发件名称', 'Sender name'],
    ['smtp_use_tls', 'SMTP TLS', 'SMTP TLS'],
    ['balance_low_notify_enabled', '余额不足通知', 'Low balance notifications'],
    ['balance_low_notify_threshold', '余额通知阈值', 'Low balance threshold'],
    ['subscription_expiry_notify_enabled', '订阅到期通知', 'Subscription expiry notifications'],
    ['account_quota_notify_enabled', '账号配额通知', 'Account quota notifications'],
  ],
}

function display(value: unknown): string {
  if (typeof value === 'boolean') return value ? t('common.enabled') : t('common.disabled')
  if (Array.isArray(value) && value.every(item => typeof item !== 'object')) return value.join(', ') || '—'
  if (value != null && typeof value === 'object') return JSON.stringify(value, null, 2)
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
