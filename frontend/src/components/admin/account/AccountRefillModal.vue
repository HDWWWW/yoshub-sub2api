<template>
  <BaseDialog :show="show" :title="t('accountRefill.title')" width="extra-wide" :close-on-escape="!busy" :show-close-button="!busy" @close="close">
    <div v-if="loading" class="py-12 text-center" role="status">{{ t('common.loading') }}</div>
    <div v-else-if="view && form" class="space-y-5">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <div v-for="item in walletItems" :key="item.key" class="rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-700">
          <div class="text-xs text-gray-500 dark:text-dark-300">{{ t(`accountRefill.${item.key}`) }}</div>
          <div class="mt-1 text-2xl font-semibold tabular-nums text-gray-900 dark:text-white">{{ number(item.value) }} <span class="text-xs font-normal text-gray-500">{{ t('accountRefill.points') }}</span></div>
        </div>
      </div>
      <p class="text-xs text-gray-500">{{ t('accountRefill.snapshot') }} {{ date(view.identity_at) }}</p>
      <div class="rounded-lg bg-gray-50 px-4 py-3 text-sm dark:bg-dark-800" aria-live="polite">{{ view.message || t('accountRefill.unconfigured') }}</div>
      <nav class="flex gap-1 overflow-x-auto border-b border-gray-200 dark:border-dark-700" :aria-label="t('accountRefill.title')">
        <button v-for="tab in tabs" :key="tab" type="button" class="whitespace-nowrap border-b-2 px-4 py-3 text-sm" :class="activeTab === tab ? 'border-primary-500 text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500'" :aria-pressed="activeTab === tab" @click="activeTab = tab">{{ t(`accountRefill.${tab}`) }}</button>
      </nav>
      <form id="refill-settings" class="space-y-5" @submit.prevent="save">
        <fieldset :disabled="busy" class="space-y-5">
          <div v-show="activeTab === 'connection'" class="space-y-5">
            <label class="block"><span class="input-label">{{ t('accountRefill.token') }} · {{ t(view.token_configured ? 'accountRefill.configured' : 'accountRefill.unconfigured') }}</span>
              <input v-model="token" type="password" autocomplete="new-password" class="input" placeholder="rst_…" data-testid="refill-token" />
              <span class="input-hint">{{ t('accountRefill.tokenHint') }}</span>
            </label>
            <dl class="grid gap-4 rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800 sm:grid-cols-2">
              <div><dt class="text-gray-500">{{ t('accountRefill.keyLabel') }}</dt><dd class="mt-1">{{ view.key ? `${view.key.label} / ${view.key.scope}` : t('accountRefill.unknown') }}</dd></div>
              <div><dt class="text-gray-500">{{ t('accountRefill.keyExpiry') }}</dt><dd class="mt-1">{{ date(view.key?.expires_at) }} <span :class="keyDays !== null && keyDays < 7 ? 'text-amber-600' : 'text-gray-500'">{{ keyRemaining }}</span></dd></div>
            </dl>
            <label class="block"><span class="input-label">{{ t('accountRefill.group_id') }}</span><select v-model.number="form.group_id" class="input" data-testid="refill-group" @change="includeMonitor"><option :value="0" disabled>{{ t('accountRefill.chooseGroup') }}</option><option v-for="group in openAIGroups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }})</option></select></label>
            <div><p class="input-label">{{ t('accountRefill.deliveryGroups') }}</p><div class="grid gap-3 sm:grid-cols-2"><label v-for="group in openAIGroups" :key="group.id" class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-700"><input v-model="form.delivery_group_ids" type="checkbox" :value="group.id" :disabled="group.id === form.group_id" />{{ group.name }} (#{{ group.id }})</label></div><p class="input-hint">{{ t('accountRefill.groupsHint') }}</p></div>
            <p class="rounded-lg bg-primary-50 p-4 text-sm leading-6 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200">{{ t('accountRefill.importHint') }}</p>
          </div>
          <div v-show="activeTab === 'rules'" class="space-y-5">
            <div class="flex flex-wrap gap-6"><label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" data-testid="refill-enabled" />{{ t('accountRefill.enabled') }}</label><label class="flex items-center gap-2 text-sm"><input v-model="form.monitor_enabled" type="checkbox" />{{ t('accountRefill.monitor_enabled') }}</label></div>
            <p class="text-sm leading-6 text-amber-700 dark:text-amber-300">{{ t('accountRefill.enableHint') }}</p>
            <div class="grid gap-4 sm:grid-cols-2">
              <label><span class="input-label">{{ t('accountRefill.mode') }}</span><select v-model="form.mode" class="input"><option v-for="mode in ['capacity', 'fixed', 'interval']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.plan') }}</span><select v-model="form.plan" class="input"><option value="pro5x">Pro5x</option><option value="pro20x">Pro20x</option></select></label>
            </div>
            <div class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800"><p>{{ t('accountRefill.quote') }}: <strong>{{ number(quote?.price) }}</strong> {{ t('accountRefill.points') }}</p><p v-if="quote" class="mt-2 text-gray-500">{{ t('accountRefill.terms') }}: {{ quote.terms }}</p></div>
            <p class="text-sm leading-6 text-gray-500">{{ t(form.mode === 'interval' ? 'accountRefill.intervalHint' : form.mode === 'fixed' ? 'accountRefill.fixedHint' : 'accountRefill.ruleHint') }}</p>
            <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><label v-for="field in ruleFields" :key="field.key"><span class="input-label">{{ t(`accountRefill.${field.key}`) }}</span><input v-model.number="form[field.key]" type="number" :min="field.min" :max="field.max" :step="field.step || 1" class="input" /></label></div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.window_enabled" type="checkbox" />{{ t('accountRefill.window_enabled') }}</label>
            <div v-if="form.window_enabled" class="grid gap-4 sm:grid-cols-2"><label><span class="input-label">{{ t('accountRefill.window_start') }}</span><input v-model="form.window_start" type="time" class="input" /></label><label><span class="input-label">{{ t('accountRefill.window_end') }}</span><input v-model="form.window_end" type="time" class="input" /></label></div>
            <p class="input-hint">{{ t('accountRefill.windowHint') }}</p>
          </div>
          <div v-show="activeTab === 'delivery'" class="space-y-5">
            <label class="flex items-center gap-2 text-sm"><input v-model="form.push_enabled" type="checkbox" />{{ t('accountRefill.push_enabled') }}</label><p class="input-hint">{{ t('accountRefill.pushHint') }}</p>
            <div class="grid gap-4 sm:grid-cols-2">
              <label><span class="input-label">{{ t('accountRefill.ws_mode') }}</span><select v-model="form.delivery.ws_mode" class="input"><option v-for="mode in ['off', 'ctx_pool', 'http_bridge', 'passthrough']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.fingerprint_mode') }}</span><select v-model="form.delivery.fingerprint_mode" class="input"><option v-for="mode in ['off', 'session', 'full']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.proxy_mode') }}</span><select v-model="form.delivery.proxy_mode" class="input" @change="form.delivery.proxy_id = null"><option v-for="mode in ['direct', 'specified', 'random']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label v-if="form.delivery.proxy_mode === 'specified'"><span class="input-label">{{ t('accountRefill.proxy_id') }}</span><input v-model.number="form.delivery.proxy_id" type="number" min="1" step="1" class="input" /></label>
              <label><span class="input-label">{{ t('accountRefill.import_concurrency') }}</span><input v-model.number="form.import_concurrency" type="number" min="1" max="10000" step="1" class="input" /></label>
              <label v-for="field in deliveryFields" :key="field.key"><span class="input-label">{{ t(`accountRefill.${field.key}`) }}</span><input v-model.number="form.delivery[field.key]" type="number" min="0" :max="field.max" :step="field.step" class="input" /></label>
              <label><span class="input-label">{{ t('accountRefill.status') }}</span><select v-model="form.delivery.status" class="input"><option value="active">{{ t('accountRefill.active') }}</option><option value="inactive">{{ t('accountRefill.inactive') }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.expiry_mode') }}</span><select v-model="form.delivery.expiry_mode" class="input"><option value="rental">{{ t('accountRefill.rental') }}</option><option value="fixed">{{ t('accountRefill.fixedExpiry') }}</option></select></label>
              <label v-if="form.delivery.expiry_mode === 'fixed'"><span class="input-label">{{ t('accountRefill.fixed_expires_at') }}</span><input v-model="fixedExpiry" type="datetime-local" class="input" /></label>
            </div>
            <div class="flex flex-wrap gap-6"><label class="flex items-center gap-2 text-sm"><input v-model="form.delivery.schedulable" type="checkbox" />{{ t('accountRefill.schedulable') }}</label><label class="flex items-center gap-2 text-sm"><input v-model="form.delivery.auto_pause_on_expired" type="checkbox" />{{ t('accountRefill.auto_pause_on_expired') }}</label></div>
            <p class="input-hint">{{ t('accountRefill.expiryHint') }}</p>
            <label class="block"><span class="input-label">{{ t('accountRefill.name_template') }}</span><input v-model="form.delivery.name_template" class="input" maxlength="256" /></label>
            <label class="block"><span class="input-label">{{ t('accountRefill.notes_template') }}</span><textarea v-model="form.delivery.notes_template" class="input" rows="2" maxlength="2000"></textarea></label>
            <p class="input-hint">{{ t('accountRefill.templateHint') }} <code v-pre>{name} {email} {id} {plan_type} {push_time} {expire_time}</code></p>
            <label class="block"><span class="input-label">{{ t('accountRefill.model_mapping') }}</span><textarea v-model="mapping" class="input font-mono text-sm" rows="4" spellcheck="false" placeholder='{"model-a": "model-b"}'></textarea></label>
          </div>
        </fieldset>
      </form>
      <div v-if="activeTab === 'progress'" class="space-y-5">
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-4"><div v-for="item in usageItems" :key="item.key" class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800"><div class="text-xs text-gray-500">{{ t(`accountRefill.${item.key}`) }}</div><div class="mt-1 font-semibold tabular-nums">{{ number(item.value) }}</div></div></div>
        <p class="text-xs leading-5 text-gray-500">{{ t('accountRefill.progressHint') }}</p>
        <div class="flex flex-wrap gap-5 text-sm"><span>{{ t('accountRefill.nextCheck') }}: {{ date(view.next_check_at) }}</span><span v-if="view.config.mode === 'interval'">{{ t('accountRefill.nextInterval') }}: {{ date(view.next_interval_at) }}</span></div>
        <div class="overflow-x-auto"><table class="w-full text-left text-sm"><thead class="border-b border-gray-200 text-xs text-gray-500 dark:border-dark-700"><tr><th class="p-2">{{ t('accountRefill.task') }}</th><th class="p-2">{{ t('accountRefill.status') }}</th><th class="p-2">{{ t('accountRefill.points') }}</th><th class="p-2">{{ t('accountRefill.elapsed') }}</th><th class="p-2">{{ t('accountRefill.created') }}</th></tr></thead><tbody><tr v-for="job in view.jobs" :key="job.id" class="border-b border-gray-100 align-top dark:border-dark-800"><td class="p-2"><span class="font-medium">{{ job.plan }}</span><div class="max-w-48 break-all text-xs text-gray-500">{{ job.supplier_id || job.id }}</div><div v-if="job.account_id" class="text-xs">{{ t('accountRefill.account') }} #{{ job.account_id }}</div></td><td class="max-w-80 p-2"><span class="font-medium">{{ stateLabel(job.status) }}</span><div v-if="job.error" class="mt-1 text-xs text-amber-700 dark:text-amber-300">{{ job.error }}</div><button v-if="['blocked', 'import_failed'].includes(job.status)" type="button" class="mt-2 text-xs text-primary-600 underline" :disabled="busy" :title="t('accountRefill.retryHint')" @click="retry(job.id)">{{ t('accountRefill.retry') }}</button><details v-if="job.status === 'blocked' && !job.supplier_id" class="mt-2 text-xs"><summary class="cursor-pointer text-amber-700">{{ t('accountRefill.dismiss') }}</summary><label class="mt-2 flex gap-2"><input v-model="confirmedJobs" type="checkbox" :value="job.id" />{{ t('accountRefill.dismissHint') }}</label><button type="button" class="btn btn-secondary mt-2 text-xs" :disabled="busy || !confirmedJobs.includes(job.id)" @click="dismiss(job.id)">{{ t('accountRefill.dismiss') }}</button></details></td><td class="p-2 tabular-nums">{{ number(job.price) }}</td><td class="whitespace-nowrap p-2 tabular-nums">{{ elapsed(job) }}</td><td class="whitespace-nowrap p-2 text-xs">{{ date(job.created_at) }}</td></tr></tbody></table><p v-if="!view.jobs.length" class="py-6 text-center text-sm text-gray-500">{{ t('accountRefill.noJobs') }}</p></div>
        <h4 class="text-sm font-medium">{{ t('accountRefill.evaluation') }}</h4>
        <div class="overflow-x-auto"><table class="w-full text-left text-sm"><thead class="border-b text-xs text-gray-500 dark:border-dark-700"><tr><th class="p-2">{{ t('accountRefill.account') }}</th><th class="p-2">{{ t('accountRefill.status') }}</th><th class="p-2">{{ t('accountRefill.sourceExpiry') }}</th><th class="p-2">{{ t('accountRefill.localExpiry') }}</th><th class="p-2">{{ t('accountRefill.remaining5h') }}</th><th class="p-2">{{ t('accountRefill.remaining7d') }}</th></tr></thead><tbody><tr v-for="account in view.evaluation.accounts" :key="account.id" class="border-b border-gray-100 dark:border-dark-800"><td class="max-w-48 break-all p-2">#{{ account.id }} {{ account.name }}</td><td class="p-2">{{ stateLabel(account.state) }}</td><td class="whitespace-nowrap p-2 text-xs">{{ date(account.source_expires_at) }}</td><td class="whitespace-nowrap p-2 text-xs">{{ date(account.expires_at) }}</td><td class="p-2">{{ account.remaining_5h === null ? '—' : `${number(account.remaining_5h)}%` }}</td><td class="p-2">{{ account.remaining_7d === null ? '—' : `${number(account.remaining_7d)}%` }}</td></tr></tbody></table><p v-if="!view.evaluation.accounts?.length" class="py-4 text-center text-sm text-gray-500">{{ t('accountRefill.noAccounts') }}</p></div>
      </div>
      <p v-if="conflict" class="text-sm text-amber-700" role="alert">{{ t('accountRefill.conflict') }}</p>
    </div>
    <p v-if="error" class="mt-3 text-sm text-red-600 dark:text-red-400" role="alert">{{ error }}</p>
    <template #footer>
      <span class="mr-auto text-xs text-gray-500">{{ dirty ? t('accountRefill.dirty') : '' }}</span>
      <button type="button" class="btn btn-secondary" :disabled="busy || loading" @click="refresh(false)">{{ t('accountRefill.refresh') }}</button>
      <button type="button" class="btn btn-secondary" :disabled="busy || !view || dirty" :title="t('accountRefill.checkHint')" @click="check">{{ t('accountRefill.check') }}</button>
      <button type="submit" form="refill-settings" class="btn btn-primary" :disabled="busy || !dirty || conflict || !form" data-testid="refill-save">{{ t(busy ? 'accountRefill.saving' : 'accountRefill.save') }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { accountRefillAPI, type RefillConfig, type RefillView, type RefillJob } from '@/api/admin/accountRefill'
import { getAll } from '@/api/admin/groups'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores/app'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: [] }>()
const { t, te } = useI18n()
const app = useAppStore()
const tabs = ['connection', 'rules', 'delivery', 'progress'] as const
const activeTab = ref<typeof tabs[number]>('connection')
const view = ref<RefillView | null>(null)
const form = ref<RefillConfig | null>(null)
const groups = ref<AdminGroup[]>([])
const confirmedJobs = ref<string[]>([])
const token = ref(''), mapping = ref('{}'), saved = ref(''), error = ref('')
const loading = ref(false), busy = ref(false), conflict = ref(false), now = ref(Date.now())
let revision = 0, generation = 0, fetching = false
let timer: ReturnType<typeof setInterval> | undefined
const openAIGroups = computed(() => groups.value.filter(g => g.platform === 'openai' && g.status === 'active'))
const dirty = computed(() => !!form.value && (JSON.stringify(form.value) !== saved.value || token.value !== '' || mapping.value !== JSON.stringify(form.value.delivery.model_mapping || {}, null, 2)))
const walletItems = computed(() => [{ key: 'balance', value: view.value?.wallet?.balance }, { key: 'held', value: view.value?.wallet?.held }, { key: 'available', value: view.value?.wallet?.available ?? view.value?.catalog?.available }])
const usageItems = computed(() => {
  const v = view.value
  return [{ key: 'spent', value: v?.daily.spent_points }, { key: 'reserved', value: v?.daily.reserved_points }, { key: 'count', value: v?.daily.count }, { key: 'budget', value: v?.config.daily_max_points }, { key: 'ready', value: v?.evaluation.ready }, { key: 'expiring', value: v?.evaluation.expiring }, { key: 'pending', value: v?.evaluation.pending }, { key: 'needed', value: v?.evaluation.needed }]
})
const quote = computed(() => view.value?.catalog?.items.find(q => q.service === 'refill' && q.plan === form.value?.plan && q.enabled))
const keyDays = computed(() => { const at = view.value?.key?.expires_at; return at && !at.startsWith('0001') ? (Date.parse(at) - now.value) / 86400000 : null })
const keyRemaining = computed(() => keyDays.value === null ? '' : keyDays.value <= 0 ? t('accountRefill.expired') : t('accountRefill.keyRemaining', { days: Math.floor(keyDays.value * 10) / 10 }))
const fixedExpiry = computed({ get: () => form.value?.delivery.fixed_expires_at ? new Date(form.value.delivery.fixed_expires_at * 1000 + 8 * 3600000).toISOString().slice(0, 16) : '', set: v => { if (form.value) form.value.delivery.fixed_expires_at = v ? Math.floor(Date.parse(`${v}:00+08:00`) / 1000) : null } })
type NumericKey = { [K in keyof RefillConfig]: RefillConfig[K] extends number ? K : never }[keyof RefillConfig]
const ruleFields = computed<{ key: NumericKey; min: number; max: number; step?: number }[]>(() => [
  ...(['min_ready', 'max_ready'] as const).map(key => ({ key, min: 1, max: 100 })),
  { key: 'lead_minutes', min: 0, max: 1440 },
  ...(['min_remaining_5h', 'min_remaining_7d'] as const).map(key => ({ key, min: 0, max: 100, step: 0.1 })),
  ...(form.value?.mode === 'capacity' ? [{ key: 'target_concurrency_percent' as const, min: 1, max: 100 }] : []),
  ...(form.value?.mode === 'interval' ? [{ key: 'interval_minutes' as const, min: 1, max: 10080 }, { key: 'interval_quantity' as const, min: 1, max: 100 }] : []),
  { key: 'check_seconds', min: 15, max: 3600 }, { key: 'cooldown_seconds', min: 60, max: 86400 }, { key: 'max_in_flight', min: 1, max: 20 }, { key: 'daily_max_count', min: 1, max: 1000 }, { key: 'daily_max_points', min: 0.01, max: 1e7, step: 0.01 }, { key: 'max_unit_price', min: 0, max: 1e7, step: 0.01 }
])
const deliveryFields = [{ key: 'priority', max: 2147483647, step: 1 }, { key: 'load_factor', max: 10000, step: 1 }, { key: 'rate_multiplier', max: 1000, step: 0.01 }] as const
const number = (v: number | null | undefined) => v == null ? '—' : v.toLocaleString(undefined, { maximumFractionDigits: 8 })
const date = (v: string | null | undefined) => !v || v.startsWith('0001') ? '—' : new Date(v).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })
const stateLabel = (state: string) => te(`accountRefill.states.${state}`) ? t(`accountRefill.states.${state}`) : state
const elapsed = (job: RefillJob) => { const seconds = Math.max(0, Math.floor(((job.finished_at ? Date.parse(job.finished_at) : now.value) - Date.parse(job.created_at)) / 1000)); return `${Math.floor(seconds / 60)}m ${seconds % 60}s` }
function includeMonitor() { if (form.value && !form.value.delivery_group_ids.includes(form.value.group_id)) form.value.delivery_group_ids.push(form.value.group_id) }
function accept(data: RefillView, reset: boolean) {
  if (!reset && data.revision < revision) return
  if (!reset && dirty.value && data.revision !== revision) conflict.value = true
  if (reset || !dirty.value) {
    form.value = JSON.parse(JSON.stringify(data.config))
    mapping.value = JSON.stringify(data.config.delivery.model_mapping || {}, null, 2)
    saved.value = JSON.stringify(form.value)
    revision = data.revision
    conflict.value = false
  }
  view.value = data
  now.value = Date.now()
}
function message(e: unknown) { const v = e as { response?: { data?: { message?: string; error?: string } } }; return v.response?.data?.message || v.response?.data?.error || t('accountRefill.error') }
async function refresh(reset: boolean) {
  if (fetching || busy.value) return
  const current = generation
  fetching = true
  try { const data = await accountRefillAPI.get(); if (current === generation) { accept(data, reset); error.value = '' } }
  catch { if (current === generation) error.value = t('accountRefill.loadError') }
  finally { fetching = false }
}
async function save() {
  if (!form.value || busy.value || conflict.value) return
  let parsed: unknown
  try { parsed = JSON.parse(mapping.value); if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object' || Object.values(parsed).some(v => typeof v !== 'string')) throw new Error() }
  catch { error.value = t('accountRefill.mappingError'); activeTab.value = 'delivery'; return }
  busy.value = true; error.value = ''
  try {
    const config = JSON.parse(JSON.stringify(form.value)) as RefillConfig
    config.delivery.model_mapping = parsed as Record<string, string>
    const data = await accountRefillAPI.save(config, token.value, revision)
    token.value = ''; accept(data, true); app.showSuccess(t('accountRefill.saved'))
  } catch (e) { error.value = message(e) }
  finally { busy.value = false }
}
async function check() { if (busy.value || dirty.value) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.check(), false) } catch (e) { error.value = message(e) } finally { busy.value = false } }
async function retry(id: string) { if (busy.value) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.retry(id), false) } catch (e) { error.value = message(e) } finally { busy.value = false } }
async function dismiss(id: string) { if (busy.value || !confirmedJobs.value.includes(id)) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.dismiss(id), false); confirmedJobs.value = confirmedJobs.value.filter(v => v !== id) } catch (e) { error.value = message(e) } finally { busy.value = false } }
function close() { if (!busy.value) emit('close') }
watch(() => props.show, async show => {
  const current = ++generation
  if (timer) clearInterval(timer)
  token.value = ''
  if (!show) return
  loading.value = true; error.value = ''; form.value = null; view.value = null; activeTab.value = 'connection'
  try { const [data, allGroups] = await Promise.all([accountRefillAPI.get(), getAll('openai')]); if (current === generation) { groups.value = allGroups; accept(data, true) } }
  catch { if (current === generation) error.value = t('accountRefill.loadError') }
  finally { if (current === generation) loading.value = false }
  if (current === generation) timer = setInterval(() => { now.value = Date.now(); void refresh(false) }, 15000)
}, { immediate: true })
onBeforeUnmount(() => { generation++; token.value = ''; if (timer) clearInterval(timer) })
</script>
