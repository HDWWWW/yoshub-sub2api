<template>
  <button type="button" class="btn btn-secondary" data-testid="open-risk-detection" @click="open">
    <Icon name="shield" size="sm" />{{ t('accountRisk.title') }}
  </button>
  <BaseDialog :show="show" :title="t('accountRisk.title')" width="wide" @close="close">
    <div class="space-y-5">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('accountRisk.scope') }}</p>
      <label class="block">
        <span class="input-label">{{ t('accountRisk.group') }}</span>
        <select v-model.number="groupID" class="input" data-testid="risk-group" :disabled="saving || testing" @change="load">
          <option :value="0" disabled>{{ t('accountRisk.chooseGroup') }}</option>
          <option v-for="group in openAIGroups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </label>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <form v-admin-read-only-form v-if="draft && view" class="space-y-5" @submit.prevent="save">
        <fieldset :disabled="saving || loading || testing" class="space-y-5">
          <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
            <input v-model="draft.enabled" data-testid="risk-enabled" type="checkbox" class="rounded" />
            <span class="text-sm font-medium">{{ t('accountRisk.enabled') }}</span>
          </label>
          <section class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-notification-settings">
            <label class="flex items-center gap-3"><input v-model="draft.notify_enabled" data-testid="risk-notify-enabled" type="checkbox" class="rounded" /><span class="text-sm font-medium">{{ t('accountRisk.notifyEnabled') }}</span></label>
            <p class="text-xs text-gray-500">{{ t('accountRisk.notifyHint') }}</p>
            <p class="text-xs" :class="view.webhook_status === 'ready' ? 'text-green-600' : 'text-amber-600'" data-testid="risk-webhook-status">{{ t(`accountRisk.webhook.${view.webhook_status || 'unavailable'}`) }}</p>
          </section>
          <section class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <label class="flex items-center gap-3">
              <input v-model="draft.notify_401_enabled" data-testid="risk-notify-401-enabled" aria-describedby="risk-notify-401-hint" type="checkbox" class="rounded" />
              <span class="text-sm font-medium">{{ t('accountRisk.notify401Title') }}</span>
            </label>
            <p id="risk-notify-401-hint" class="text-xs text-gray-500">{{ t('accountRisk.notify401Hint') }}</p>
            <label class="flex items-center gap-3">
              <input v-model="draft.probe_401_enabled" data-testid="risk-probe-401-enabled" aria-describedby="risk-probe-401-hint" type="checkbox" class="rounded" />
              <span class="text-sm font-medium">{{ t('accountRisk.probe401Title') }}</span>
            </label>
            <p id="risk-probe-401-hint" class="text-xs text-gray-500">{{ t('accountRisk.probe401Hint') }}</p>
          </section>
          <section class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-model-mismatch-settings">
            <label class="block">
              <span class="input-label">{{ t('accountRisk.modelMismatchTitle') }}</span>
              <select v-model="draft.model_mismatch_action" class="input" data-testid="risk-model-mismatch-action">
                <option value="off">{{ t('accountRisk.modelMismatchOff') }}</option>
                <option value="stop">{{ t('accountRisk.modelMismatchStop') }}</option>
                <option value="notify_only">{{ t('accountRisk.modelMismatchNotifyOnly') }}</option>
              </select>
            </label>
            <p class="text-xs text-gray-500">{{ t('accountRisk.modelMismatchHint') }}</p>
          </section>
          <section class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <label class="flex items-center gap-3">
              <input v-model="burst429Action" true-value="stop" false-value="off" data-testid="risk-429-enabled" type="checkbox" class="rounded" />
              <span class="text-sm font-semibold">{{ t('accountRisk.rateLimit429Title') }}</span>
            </label>
            <label class="flex items-center gap-3">
              <input v-model="burst429Action" true-value="reduce" false-value="off" data-testid="risk-429-reduce" type="checkbox" class="rounded" />
              <span class="text-sm font-semibold">{{ t('accountRisk.rateLimit429Reduce') }}</span>
            </label>
            <div class="grid gap-3 sm:grid-cols-2">
              <label><span class="input-label">{{ t('accountRisk.rateLimit429Window') }}</span><input v-model.number="draft.rate_limit_429_window_seconds" class="input" data-testid="risk-429-window" type="number" min="1" max="3600" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.rateLimit429Threshold') }}</span><input v-model.number="draft.rate_limit_429_threshold" class="input" data-testid="risk-429-threshold" type="number" min="1" max="10000" step="1" required /></label>
            </div>
            <template v-if="draft.rate_limit_429_reduce_concurrency">
              <label class="block sm:w-1/2">
                <span class="input-label">{{ t('accountRisk.rateLimit429Limit') }}</span>
                <input v-model.number="draft.rate_limit_429_concurrency_limit" class="input" data-testid="risk-429-limit" aria-describedby="risk-429-limit-hint" type="number" min="1" max="2147483647" step="1" required />
              </label>
              <p id="risk-429-limit-hint" class="text-xs text-gray-500">{{ t('accountRisk.rateLimit429LimitHint') }}</p>
            </template>
            <p class="text-xs text-gray-500">
              <template v-if="valid429Window">{{ t('accountRisk.rateLimit429Trigger', { window: draft.rate_limit_429_window_seconds, count: draft.rate_limit_429_threshold + 1 }) }} </template>
              {{ t('accountRisk.rateLimit429Hint') }}
            </p>
          </section>
          <section class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-custom-errors">
            <label class="flex items-center gap-3"><input v-model="draft.custom_error_enabled" type="checkbox" class="rounded" data-testid="risk-custom-enabled" @change="initializeErrorMatch" /><span class="text-sm font-semibold">{{ t('accountRisk.customTitle') }}</span></label>
            <template v-if="draft.custom_error_enabled">
              <div class="grid gap-3 sm:grid-cols-2">
                <label><span class="input-label">{{ t('accountRisk.rateLimit429Window') }}</span><input v-model.number="draft.custom_error_window_seconds" class="input" type="number" min="1" max="3600" required data-testid="risk-custom-window" /></label>
                <label><span class="input-label">{{ t('accountRisk.rateLimit429Threshold') }}</span><input v-model.number="draft.custom_error_threshold" class="input" type="number" min="1" max="10000" required data-testid="risk-custom-threshold" /></label>
              </div>
              <div v-for="(rule, i) in draft.custom_error_rules" :key="i" class="flex flex-wrap items-end gap-2">
                <label class="w-36"><span class="input-label">{{ t('accountRisk.customStatus') }}</span><input :value="rule.status" class="input" type="number" min="400" max="599" placeholder="503" :data-testid="`risk-custom-status-${i}`" @input="rule.status = ($event.target as HTMLInputElement).valueAsNumber || undefined" /></label>
                <label class="min-w-40 flex-1"><span class="input-label">{{ t('accountRisk.customKeyword') }}</span><input v-model.trim="rule.keyword" class="input" maxlength="128" placeholder="overload" :data-testid="`risk-custom-keyword-${i}`" /></label>
                <button type="button" class="btn btn-secondary" @click="draft.custom_error_rules.splice(i, 1)">{{ t('accountRisk.customRemove') }}</button>
              </div>
              <button type="button" class="btn btn-secondary" data-testid="risk-custom-add" :disabled="draft.custom_error_rules.length >= 20" @click="draft.custom_error_rules.push({})">{{ t('accountRisk.customAdd') }}</button>
              <p class="text-xs text-gray-500">{{ t('accountRisk.customHint') }}</p>
            </template>
          </section>
          <section class="space-y-3">
            <h3 class="text-sm font-semibold">{{ t('accountRisk.multiTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountRisk.multiHint') }}</p>
            <div class="grid gap-3 sm:grid-cols-3">
              <label><span class="input-label">{{ t('accountRisk.firstThreshold') }}</span><input v-model.number="draft.first_below_percent" class="input" data-testid="risk-first" type="number" min="1" max="100" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.secondThreshold') }}</span><input v-model.number="draft.second_at_least_percent" class="input" data-testid="risk-second" type="number" min="1" max="100" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.duration') }}</span><input v-model.number="draft.sustain_seconds" class="input" data-testid="risk-duration" type="number" min="1" max="3600" step="1" required /></label>
            </div>
            <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
              <input v-model="draft.capacity_notify_only" data-testid="risk-capacity-notify-only" type="checkbox" class="rounded" />
              <span class="text-sm">{{ t('accountRisk.capacityNotifyOnly') }}</span>
            </label>
          </section>
          <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
            <h3 class="text-sm font-semibold">{{ t('accountRisk.singleTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountRisk.singleHint') }}</p>
            <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
              <input v-model="draft.probe_enabled" data-testid="risk-probe-enabled" type="checkbox" class="rounded" />
              <span class="text-sm font-medium">{{ t('accountRisk.probeEnabled') }}</span>
            </label>
            <label class="block sm:max-w-sm">
              <span class="input-label">{{ t('accountRisk.probeScope') }}</span>
              <select v-model="draft.probe_scope" class="input" data-testid="risk-probe-scope" :disabled="!draft.probe_enabled">
                <option value="single">{{ t('accountRisk.probeScopeSingle') }}</option>
                <option value="all">{{ t('accountRisk.probeScopeAll') }}</option>
              </select>
            </label>
            <p class="text-xs text-gray-500">{{ t('accountRisk.probeScopeHint') }}</p>
            <div class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-probe-mismatch-settings">
              <label class="block sm:max-w-xs">
                <span class="input-label">{{ t('accountRisk.probeMismatchMaxRetries') }}</span>
                <input v-model.number="draft.probe_mismatch_max_retries" class="input" data-testid="risk-probe-mismatch-retries" type="number" min="0" max="20" step="1" :disabled="!draft.probe_enabled" required />
              </label>
              <p class="text-xs text-gray-500">{{ t('accountRisk.probeMismatchHint') }}</p>
            </div>
            <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
              <input v-model="draft.probe_notify_only" data-testid="risk-probe-notify-only" type="checkbox" class="rounded" />
              <span class="text-sm">{{ t('accountRisk.probeNotifyOnly') }}</span>
            </label>
            <div class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-probe-429-settings">
              <label class="flex items-center gap-3">
                <input v-model="draft.probe_429_stop_enabled" data-testid="risk-probe-429-enabled" type="checkbox" class="rounded" />
                <span class="text-sm font-medium">{{ t('accountRisk.probe429StopEnabled') }}</span>
              </label>
              <label class="block sm:max-w-xs">
                <span class="input-label">{{ t('accountRisk.probe429MaxRetries') }}</span>
                <input v-model.number="draft.probe_429_max_retries" class="input" data-testid="risk-probe-429-retries" type="number" min="1" max="20" step="1" :disabled="!draft.probe_429_stop_enabled" required />
              </label>
              <p class="text-xs text-gray-500">{{ t('accountRisk.probe429Hint') }}</p>
            </div>
            <div class="grid gap-3 sm:grid-cols-3">
              <label><span class="input-label">{{ t('accountRisk.interval') }}</span><input v-model.number="draft.probe_interval_seconds" class="input" data-testid="risk-interval" type="number" min="10" max="86400" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.model') }}</span><input v-model="draft.model" class="input" data-testid="risk-model" list="risk-model-options" maxlength="200" required /><datalist id="risk-model-options"><option value="gpt-6.1-sol" /><option value="gpt-6-astra" /><option value="gpt-5.4" /></datalist></label>
              <label><span class="input-label">{{ t('accountRisk.effort') }}</span><select v-model="draft.reasoning_effort" class="input" data-testid="risk-effort"><option value="">{{ t('accountRisk.upstreamDefault') }}</option><option v-for="effort in efforts" :key="effort" :value="effort">{{ effort }}</option></select></label>
            </div>
            <label class="block"><span class="input-label">{{ t('accountRisk.prompt') }}</span><textarea v-model="draft.probe_prompt" class="input min-h-20" data-testid="risk-prompt" rows="3" maxlength="8192" required /></label>
            <label class="block"><span class="input-label">{{ t('accountRisk.expected') }}</span><input v-model="draft.expected_text" class="input" data-testid="risk-expected" maxlength="256" required /></label>
          </section>
          <section class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <h3 class="text-sm font-semibold">{{ t('accountRisk.manualTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountRisk.manualHint') }}</p>
            <label class="block"><span class="input-label">{{ t('accountRisk.manualAccount') }}</span><select v-model.number="testAccountID" class="input" data-testid="risk-test-account"><option :value="0" disabled>{{ t('accountRisk.chooseAccount') }}</option><option v-for="account in view.test_accounts || []" :key="account.id" :value="account.id">#{{ account.id }} {{ account.name }} · {{ account.schedulable ? t('accountRisk.schedulingOn') : t('accountRisk.schedulingOff') }}</option></select></label>
            <button v-admin-write type="button" class="btn btn-secondary" data-testid="risk-test-now" :disabled="!valid || !testAccountID || testing" @click="testNow">{{ testing ? t('accountRisk.testing') : t('accountRisk.testNow') }}</button>
            <div v-if="testResult" role="status" data-testid="risk-test-result" class="space-y-2 text-sm">
              <p :class="testResult.matched === true ? 'text-green-600' : 'text-amber-600'">{{ testResult.message }}</p>
              <pre v-if="testResult.text" class="max-h-60 overflow-auto whitespace-pre-wrap break-words rounded bg-gray-50 p-3 dark:bg-dark-700">{{ testResult.text }}</pre>
              <p v-if="testResult.truncated" class="text-xs text-gray-500">{{ t('accountRisk.replyTruncated') }}</p>
            </div>
          </section>
          <p class="text-xs text-gray-500">{{ t('accountRisk.stopHint') }}</p>
          <button v-admin-write type="submit" class="btn btn-primary" data-testid="risk-save" :disabled="!valid">{{ t('common.save') }}</button>
          <span v-if="saved" role="status" class="ml-3 text-sm text-green-600">{{ t('accountRisk.saved') }}</span>
        </fieldset>
        <section class="space-y-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
          <div class="flex items-center justify-between gap-3"><h3 class="font-semibold">{{ t('accountRisk.status') }}</h3><button type="button" class="btn btn-secondary text-xs" :disabled="refreshing || loading || saving" @click="refresh">{{ t('common.refresh') }}</button></div>
          <p>{{ view.config.enabled ? t('accountRisk.active') : t('accountRisk.disabled') }} · {{ view.observation.message }}</p>
          <p v-if="view.config.enabled && stale" class="text-amber-600">{{ t('accountRisk.stale') }}</p>
          <div v-if="view.observation.mode === 'capacity' && capacityAnomalies.length" class="space-y-2">
            <div v-for="check in capacityAnomalies" :key="check.target.account_id" class="rounded bg-gray-50 p-3 dark:bg-dark-700" data-testid="risk-capacity-pair">
              <div class="grid gap-2 sm:grid-cols-2">
                <div v-for="(item, index) in [check.target, check.reference]" :key="index">
                  <template v-if="item"><p>{{ index === 0 ? t('accountRisk.first') : t('accountRisk.second') }}: #{{ item.account_id }} {{ item.name }}</p><p class="font-mono">{{ item.used }} / {{ item.capacity }} · {{ item.percent.toFixed(1) }}%</p></template>
                </div>
              </div>
              <p v-if="check.anomaly_since" class="mt-1 text-xs text-gray-500">{{ t('accountRisk.since') }} {{ formatNotificationTime(check.anomaly_since) }}</p>
              <p v-if="check.next_probe_at" class="text-xs text-gray-500">{{ t('accountRisk.nextProbe') }} {{ formatNotificationTime(check.next_probe_at) }}</p>
            </div>
          </div>
          <div v-else-if="view.observation.mode === 'capacity'" class="grid gap-2 sm:grid-cols-2">
            <div v-for="(item, index) in [view.observation.first, view.observation.second]" :key="index" class="rounded bg-gray-50 p-2 dark:bg-dark-700">
              <template v-if="item"><p>{{ index === 0 ? t('accountRisk.first') : t('accountRisk.second') }}: #{{ item.account_id }} {{ item.name }}</p><p class="font-mono">{{ item.used }} / {{ item.capacity }} · {{ item.percent.toFixed(1) }}%</p></template>
            </div>
          </div>
          <p v-if="!capacityAnomalies.length && view.observation.anomaly_since">{{ t('accountRisk.since') }} {{ formatNotificationTime(view.observation.anomaly_since) }}</p>
          <p v-if="(view.observation.mode !== 'capacity' || !capacityAnomalies.length) && view.observation.next_probe_at">{{ t('accountRisk.nextProbe') }} {{ formatNotificationTime(view.observation.next_probe_at) }}</p>
          <p v-if="view.observation.last_probe_at">{{ formatNotificationTime(view.observation.last_probe_at) }} · {{ view.observation.last_probe_result }}</p>
          <div v-if="scheduledChecks.length" class="space-y-2" data-testid="risk-scheduled-status">
            <h4 class="font-medium">{{ t('accountRisk.scheduledAccounts') }}</h4>
            <div class="max-h-72 space-y-2 overflow-y-auto">
              <div v-for="check in scheduledChecks" :key="check.account_id" class="rounded bg-gray-50 p-3 dark:bg-dark-700" data-testid="risk-scheduled-account">
                <p>#{{ check.account_id }} {{ check.account_name }}</p>
                <p class="text-xs text-gray-500">{{ t('accountRisk.nextProbe') }} {{ formatNotificationTime(check.next_probe_at) }}</p>
                <p v-if="check.last_probe_at" class="text-xs text-gray-500">{{ formatNotificationTime(check.last_probe_at) }} · {{ check.last_probe_result }}</p>
              </div>
            </div>
          </div>
        </section>
        <section v-if="view.events.length" class="space-y-2 text-sm">
          <h3 class="font-semibold">{{ t('accountRisk.history') }}</h3>
          <div v-for="(event, index) in view.events" :key="event.id || index" class="border-l-2 border-amber-400 pl-3"><p>{{ formatNotificationTime(event.at) }} · #{{ event.account_id }}</p><p class="text-gray-500">{{ event.reason }}</p><p v-if="event.action" class="text-xs" data-testid="risk-event-action">{{ t(`accountRisk.action.${event.action}`) }}</p><p v-if="event.kind !== 'rate_limit_429' && (event.occurrences || 0) > 1" class="text-xs">{{ t('accountRisk.occurrences', { count: event.occurrences }) }}</p><p v-if="event.notification" class="text-xs" data-testid="risk-notification-result">{{ t(`accountRisk.notification.${event.notification}`) }}</p></div>
        </section>
      </form>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { accountRiskAPI, type AccountRiskConfig, type AccountRiskRule, type AccountRiskManualResult } from '@/api/admin/accountRisk'
import { formatNotificationTime } from '@/utils/beijingTime'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ groups: { id: number; name: string; platform: string }[]; initialGroup?: string | number }>()
const { t } = useI18n()
const show = ref(false), loading = ref(false), saving = ref(false), refreshing = ref(false), saved = ref(false)
const testing = ref(false), testAccountID = ref(0), testResult = ref<AccountRiskManualResult | null>(null)
const groupID = ref(0), error = ref(''), now = ref(Date.now())
const view = ref<AccountRiskRule | null>(null), draft = ref<AccountRiskConfig | null>(null)
const efforts = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh']
const openAIGroups = computed(() => props.groups.filter(g => g.platform === 'openai'))
const scheduledChecks = computed(() => Object.values(view.value?.observation.scheduled_checks ?? {})
  .sort((a, b) => Date.parse(a.next_probe_at) - Date.parse(b.next_probe_at) || a.account_id - b.account_id))
const capacityAnomalies = computed(() => Object.values(view.value?.observation.capacity_checks ?? {})
  .filter(check => check.reference && check.anomaly_since)
  .sort((a, b) => Date.parse(a.anomaly_since!) - Date.parse(b.anomaly_since!) || a.target.account_id - b.target.account_id))
function initializeErrorMatch() {
  const c = draft.value
  if (c?.custom_error_enabled && !c.custom_error_rules.length) c.custom_error_rules.push({})
  if (c && !c.custom_error_enabled) c.custom_error_rules = c.custom_error_rules.filter(r => r.status || r.keyword?.trim())
}
function valid429Limit(limit: number) {
  return Number.isInteger(limit) && limit >= 1 && limit <= 2147483647
}
const burst429Action = computed<'off' | 'stop' | 'reduce'>({
  get() {
    if (draft.value?.rate_limit_429_reduce_concurrency) return 'reduce'
    return draft.value?.rate_limit_429_enabled ? 'stop' : 'off'
  },
  set(action) {
    const c = draft.value
    if (!c) return
    c.rate_limit_429_enabled = action === 'stop'
    c.rate_limit_429_reduce_concurrency = action === 'reduce'
    if (!valid429Limit(c.rate_limit_429_concurrency_limit)) c.rate_limit_429_concurrency_limit = 1
  }
})
const valid429Window = computed(() => {
  const c = draft.value
  return !!c && Number.isInteger(c.rate_limit_429_window_seconds) && c.rate_limit_429_window_seconds >= 1 && c.rate_limit_429_window_seconds <= 3600 &&
    Number.isInteger(c.rate_limit_429_threshold) && c.rate_limit_429_threshold >= 1 && c.rate_limit_429_threshold <= 10000
})
const valid = computed(() => {
  const c = draft.value
  return !!c && valid429Window.value && (!c.rate_limit_429_reduce_concurrency || valid429Limit(c.rate_limit_429_concurrency_limit)) &&
    (!c.custom_error_enabled || (c.custom_error_rules.length > 0 &&
      Number.isInteger(c.custom_error_window_seconds) && c.custom_error_window_seconds >= 1 && c.custom_error_window_seconds <= 3600 &&
      Number.isInteger(c.custom_error_threshold) && c.custom_error_threshold >= 1 && c.custom_error_threshold <= 10000 &&
      c.custom_error_rules.every(r => (!r.status || (Number.isInteger(r.status) && r.status >= 400 && r.status <= 599)) && (r.status || r.keyword?.trim())))) &&
    ['single', 'all'].includes(c.probe_scope) &&
    Number.isInteger(c.probe_mismatch_max_retries) && c.probe_mismatch_max_retries >= 0 && c.probe_mismatch_max_retries <= 20 &&
    (!c.probe_429_stop_enabled || (Number.isInteger(c.probe_429_max_retries) && c.probe_429_max_retries >= 1 && c.probe_429_max_retries <= 20)) &&
    Number.isInteger(c.first_below_percent) && c.first_below_percent >= 1 && c.first_below_percent <= c.second_at_least_percent &&
    Number.isInteger(c.second_at_least_percent) && c.second_at_least_percent <= 100 &&
    Number.isInteger(c.sustain_seconds) && c.sustain_seconds >= 1 && c.sustain_seconds <= 3600 &&
    Number.isInteger(c.probe_interval_seconds) && c.probe_interval_seconds >= 10 && c.probe_interval_seconds <= 86400 &&
    !!c.probe_prompt.trim() && !!c.expected_text.trim() && !!c.model.trim()
})
const stale = computed(() => !view.value?.observation.checked_at || now.value - Date.parse(view.value.observation.checked_at) > 10000)
let generation = 0, timer: ReturnType<typeof setInterval> | undefined
async function open() {
  show.value = true
  groupID.value = openAIGroups.value.some(g => g.id === Number(props.initialGroup)) ? Number(props.initialGroup) : (openAIGroups.value[0]?.id ?? 0)
  if (!timer) timer = setInterval(() => { now.value = Date.now(); void refresh() }, 5000)
  await load()
}
function close() { if (saving.value || testing.value) return; show.value = false; generation++; if (timer) clearInterval(timer); timer = undefined }
async function load() {
  const current = ++generation
  view.value = null; draft.value = null; testResult.value = null; testAccountID.value = 0; error.value = ''; saved.value = false
  if (!groupID.value) return
  loading.value = true
  try { const data = await accountRiskAPI.get(groupID.value); if (current === generation) { view.value = data; draft.value = { ...data.config,
    rate_limit_429_reduce_concurrency: data.config.rate_limit_429_reduce_concurrency ?? false,
    rate_limit_429_concurrency_limit: data.config.rate_limit_429_concurrency_limit || 1,
    custom_error_enabled: data.config.custom_error_enabled ?? false, custom_error_window_seconds: data.config.custom_error_window_seconds || 60,
    custom_error_threshold: data.config.custom_error_threshold || 20, custom_error_rules: (data.config.custom_error_rules ?? []).map(r => ({ ...r })),
    rate_limit_429_enabled: data.config.rate_limit_429_enabled ?? false, rate_limit_429_window_seconds: data.config.rate_limit_429_window_seconds || 60, rate_limit_429_threshold: data.config.rate_limit_429_threshold || 20, notify_enabled: data.config.notify_enabled ?? false, capacity_notify_only: data.config.capacity_notify_only ?? false, probe_enabled: data.config.probe_enabled ?? true, probe_scope: data.config.probe_scope || 'single', probe_notify_only: data.config.probe_notify_only ?? false, probe_mismatch_max_retries: data.config.probe_mismatch_max_retries ?? 0, probe_401_enabled: data.config.probe_401_enabled ?? false, notify_401_enabled: data.config.notify_401_enabled ?? data.config.probe_401_enabled ?? false, probe_429_stop_enabled: data.config.probe_429_stop_enabled ?? false, probe_429_max_retries: data.config.probe_429_max_retries || 3, model_mismatch_action: data.config.model_mismatch_action || 'off' } } }
  catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRisk.loadError')) }
  finally { if (current === generation) loading.value = false }
}
async function refresh() {
  if (!show.value || !view.value || loading.value || saving.value || refreshing.value) return
  const current = generation
  refreshing.value = true
  // Refresh actual state without overwriting the draft or its revision guard.
  try { const data = await accountRiskAPI.get(groupID.value); if (current === generation && view.value) { view.value.config = data.config; view.value.observation = data.observation; view.value.events = data.events; view.value.webhook_status = data.webhook_status; view.value.test_accounts = data.test_accounts } }
  catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRisk.loadError')) }
  finally { refreshing.value = false }
}
async function save() {
  if (!valid.value || !view.value || !draft.value || loading.value || saving.value || testing.value) return
  generation++
  saving.value = true; error.value = ''; saved.value = false
  try { view.value = await accountRiskAPI.save(groupID.value, view.value.revision, { ...draft.value }); draft.value = { ...view.value.config }; saved.value = true }
  catch (e) { error.value = extractApiErrorMessage(e, t('accountRisk.saveError')) }
  finally { saving.value = false }
}
async function testNow() {
  if (!valid.value || !draft.value || !view.value || !testAccountID.value || testing.value || saving.value) return
  const current = generation
  testing.value = true; testResult.value = null; error.value = ''
  try {
    const result = await accountRiskAPI.test(groupID.value, testAccountID.value, view.value.revision, { ...draft.value })
    if (current === generation) testResult.value = result
  } catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRisk.testError')) }
  finally { testing.value = false }
}
onUnmounted(() => { generation++; if (timer) clearInterval(timer) })
</script>
