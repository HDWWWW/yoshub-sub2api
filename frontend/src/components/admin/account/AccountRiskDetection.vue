<template>
  <button type="button" class="btn btn-secondary" data-testid="open-risk-detection" @click="open">
    <Icon name="shield" size="sm" />{{ t('accountRisk.title') }}
  </button>
  <BaseDialog :show="show" :title="t('accountRisk.title')" width="wide" @close="close">
    <div class="space-y-5">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('accountRisk.scope') }}</p>
      <label class="block">
        <span class="input-label">{{ t('accountRisk.group') }}</span>
        <select v-model.number="groupID" class="input" data-testid="risk-group" :disabled="saving" @change="load">
          <option :value="0" disabled>{{ t('accountRisk.chooseGroup') }}</option>
          <option v-for="group in openAIGroups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </label>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <form v-if="draft && view" class="space-y-5" @submit.prevent="save">
        <fieldset :disabled="saving || loading" class="space-y-5">
          <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
            <input v-model="draft.enabled" data-testid="risk-enabled" type="checkbox" class="rounded" />
            <span class="text-sm font-medium">{{ t('accountRisk.enabled') }}</span>
          </label>
          <section class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="risk-notification-settings">
            <label class="flex items-center gap-3"><input v-model="draft.notify_enabled" data-testid="risk-notify-enabled" type="checkbox" class="rounded" /><span class="text-sm font-medium">{{ t('accountRisk.notifyEnabled') }}</span></label>
            <p class="text-xs text-gray-500">{{ t('accountRisk.notifyHint') }}</p>
            <p class="text-xs" :class="view.webhook_status === 'ready' ? 'text-green-600' : 'text-amber-600'" data-testid="risk-webhook-status">{{ t(`accountRisk.webhook.${view.webhook_status || 'unavailable'}`) }}</p>
          </section>
          <section class="space-y-3">
            <h3 class="text-sm font-semibold">{{ t('accountRisk.multiTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountRisk.multiHint') }}</p>
            <div class="grid gap-3 sm:grid-cols-3">
              <label><span class="input-label">{{ t('accountRisk.firstThreshold') }}</span><input v-model.number="draft.first_below_percent" class="input" data-testid="risk-first" type="number" min="1" max="100" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.secondThreshold') }}</span><input v-model.number="draft.second_at_least_percent" class="input" data-testid="risk-second" type="number" min="1" max="100" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.duration') }}</span><input v-model.number="draft.sustain_seconds" class="input" data-testid="risk-duration" type="number" min="1" max="3600" step="1" required /></label>
            </div>
          </section>
          <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
            <h3 class="text-sm font-semibold">{{ t('accountRisk.singleTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountRisk.singleHint') }}</p>
            <div class="grid gap-3 sm:grid-cols-3">
              <label><span class="input-label">{{ t('accountRisk.interval') }}</span><input v-model.number="draft.probe_interval_seconds" class="input" data-testid="risk-interval" type="number" min="10" max="86400" step="1" required /></label>
              <label><span class="input-label">{{ t('accountRisk.model') }}</span><input v-model="draft.model" class="input" data-testid="risk-model" list="risk-model-options" maxlength="200" required /><datalist id="risk-model-options"><option value="gpt-6.1-sol" /><option value="gpt-6-astra" /><option value="gpt-5.4" /></datalist></label>
              <label><span class="input-label">{{ t('accountRisk.effort') }}</span><select v-model="draft.reasoning_effort" class="input" data-testid="risk-effort"><option value="">{{ t('accountRisk.upstreamDefault') }}</option><option v-for="effort in efforts" :key="effort" :value="effort">{{ effort }}</option></select></label>
            </div>
            <label class="block"><span class="input-label">{{ t('accountRisk.prompt') }}</span><textarea v-model="draft.probe_prompt" class="input min-h-20" data-testid="risk-prompt" rows="3" maxlength="8192" required /></label>
            <label class="block"><span class="input-label">{{ t('accountRisk.expected') }}</span><input v-model="draft.expected_text" class="input" data-testid="risk-expected" maxlength="256" required /></label>
          </section>
          <p class="text-xs text-gray-500">{{ t('accountRisk.stopHint') }}</p>
          <button type="submit" class="btn btn-primary" data-testid="risk-save" :disabled="!valid">{{ t('common.save') }}</button>
          <span v-if="saved" role="status" class="ml-3 text-sm text-green-600">{{ t('accountRisk.saved') }}</span>
        </fieldset>
        <section class="space-y-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
          <div class="flex items-center justify-between gap-3"><h3 class="font-semibold">{{ t('accountRisk.status') }}</h3><button type="button" class="btn btn-secondary text-xs" :disabled="refreshing || loading || saving" @click="refresh">{{ t('common.refresh') }}</button></div>
          <p>{{ view.config.enabled ? t('accountRisk.active') : t('accountRisk.disabled') }} · {{ view.observation.message }}</p>
          <p v-if="view.config.enabled && stale" class="text-amber-600">{{ t('accountRisk.stale') }}</p>
          <div v-if="view.observation.mode === 'capacity'" class="grid gap-2 sm:grid-cols-2">
            <div v-for="(item, index) in [view.observation.first, view.observation.second]" :key="index" class="rounded bg-gray-50 p-2 dark:bg-dark-700">
              <template v-if="item"><p>{{ index === 0 ? t('accountRisk.first') : t('accountRisk.second') }}: #{{ item.account_id }} {{ item.name }}</p><p class="font-mono">{{ item.used }} / {{ item.capacity }} · {{ item.percent.toFixed(1) }}%</p></template>
            </div>
          </div>
          <p v-if="view.observation.anomaly_since">{{ t('accountRisk.since') }} {{ formatTime(view.observation.anomaly_since) }}</p>
          <p v-if="view.observation.next_probe_at">{{ t('accountRisk.nextProbe') }} {{ formatTime(view.observation.next_probe_at) }}</p>
          <p v-if="view.observation.last_probe_at">{{ formatTime(view.observation.last_probe_at) }} · {{ view.observation.last_probe_result }}</p>
        </section>
        <section v-if="view.events.length" class="space-y-2 text-sm">
          <h3 class="font-semibold">{{ t('accountRisk.history') }}</h3>
          <div v-for="(event, index) in view.events" :key="event.id || index" class="border-l-2 border-amber-400 pl-3"><p>{{ formatTime(event.at) }} · #{{ event.account_id }}</p><p class="text-gray-500">{{ event.reason }}</p><p v-if="event.notification" class="text-xs" data-testid="risk-notification-result">{{ t(`accountRisk.notification.${event.notification}`) }}</p></div>
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
import { accountRiskAPI, type AccountRiskConfig, type AccountRiskRule } from '@/api/admin/accountRisk'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ groups: { id: number; name: string; platform: string }[]; initialGroup?: string | number }>()
const { t } = useI18n()
const show = ref(false), loading = ref(false), saving = ref(false), refreshing = ref(false), saved = ref(false)
const groupID = ref(0), error = ref(''), now = ref(Date.now())
const view = ref<AccountRiskRule | null>(null), draft = ref<AccountRiskConfig | null>(null)
const efforts = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh']
const openAIGroups = computed(() => props.groups.filter(g => g.platform === 'openai'))
const valid = computed(() => {
  const c = draft.value
  return !!c && Number.isInteger(c.first_below_percent) && c.first_below_percent >= 1 && c.first_below_percent <= c.second_at_least_percent &&
    Number.isInteger(c.second_at_least_percent) && c.second_at_least_percent <= 100 &&
    Number.isInteger(c.sustain_seconds) && c.sustain_seconds >= 1 && c.sustain_seconds <= 3600 &&
    Number.isInteger(c.probe_interval_seconds) && c.probe_interval_seconds >= 10 && c.probe_interval_seconds <= 86400 &&
    !!c.probe_prompt.trim() && !!c.expected_text.trim() && !!c.model.trim()
})
const stale = computed(() => !view.value?.observation.checked_at || now.value - Date.parse(view.value.observation.checked_at) > 10000)
let generation = 0, timer: ReturnType<typeof setInterval> | undefined
function formatTime(value: string) { return new Date(value).toLocaleString() }
async function open() {
  show.value = true
  groupID.value = openAIGroups.value.some(g => g.id === Number(props.initialGroup)) ? Number(props.initialGroup) : (openAIGroups.value[0]?.id ?? 0)
  if (!timer) timer = setInterval(() => { now.value = Date.now(); void refresh() }, 5000)
  await load()
}
function close() { if (saving.value) return; show.value = false; generation++; if (timer) clearInterval(timer); timer = undefined }
async function load() {
  const current = ++generation
  view.value = null; draft.value = null; error.value = ''; saved.value = false
  if (!groupID.value) return
  loading.value = true
  try { const data = await accountRiskAPI.get(groupID.value); if (current === generation) { view.value = data; draft.value = { ...data.config, notify_enabled: data.config.notify_enabled ?? false } } }
  catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRisk.loadError')) }
  finally { if (current === generation) loading.value = false }
}
async function refresh() {
  if (!show.value || !view.value || loading.value || saving.value || refreshing.value) return
  const current = generation
  refreshing.value = true
  // Refresh actual state without overwriting the draft or its revision guard.
  try { const data = await accountRiskAPI.get(groupID.value); if (current === generation && view.value) { view.value.config = data.config; view.value.observation = data.observation; view.value.events = data.events; view.value.webhook_status = data.webhook_status } }
  catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRisk.loadError')) }
  finally { refreshing.value = false }
}
async function save() {
  if (!valid.value || !view.value || !draft.value || loading.value || saving.value) return
  generation++
  saving.value = true; error.value = ''; saved.value = false
  try { view.value = await accountRiskAPI.save(groupID.value, view.value.revision, { ...draft.value }); draft.value = { ...view.value.config }; saved.value = true }
  catch (e) { error.value = extractApiErrorMessage(e, t('accountRisk.saveError')) }
  finally { saving.value = false }
}
onUnmounted(() => { generation++; if (timer) clearInterval(timer) })
</script>
