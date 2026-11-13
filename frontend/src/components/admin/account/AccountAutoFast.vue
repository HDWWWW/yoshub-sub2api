<template>
  <button type="button" class="btn btn-secondary" data-testid="open-auto-fast" @click="open">
    <Icon name="bolt" size="sm" />{{ t('accountAutoFast.title') }}
  </button>
  <BaseDialog :show="show" :title="t('accountAutoFast.title')" width="wide" @close="close">
    <div class="space-y-5">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('accountAutoFast.scope') }}</p>
      <label class="block">
        <span class="input-label">{{ t('accountAutoFast.group') }}</span>
        <select v-model.number="groupID" class="input" data-testid="auto-fast-group" :disabled="saving" @change="load">
          <option :value="0" disabled>{{ t('accountAutoFast.chooseGroup') }}</option>
          <option v-for="group in openAIGroups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </label>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <form v-admin-read-only-form v-if="draft && view" class="space-y-5" @submit.prevent="save">
        <fieldset :disabled="saving || loading" class="space-y-5">
          <label class="flex items-center gap-3 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
            <input v-model="draft.enabled" data-testid="auto-fast-enabled" type="checkbox" class="rounded" />
            <span class="text-sm font-medium">{{ t('accountAutoFast.enabled') }}</span>
          </label>
          <div class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <label class="flex items-center gap-3">
              <input v-model="draft.require_multiple_accounts" data-testid="auto-fast-require-multiple" type="checkbox" class="rounded" />
              <span class="text-sm font-medium">{{ t('accountAutoFast.requireMultipleAccounts') }}</span>
            </label>
            <p class="text-xs text-gray-500">{{ t('accountAutoFast.requireMultipleAccountsHint') }}</p>
          </div>
          <section class="space-y-3">
            <h3 class="text-sm font-semibold">{{ t('accountAutoFast.stagesTitle') }}</h3>
            <p class="text-xs text-gray-500">{{ t('accountAutoFast.stagesHint') }}</p>
            <div v-for="(stage, index) in draft.stages" :key="index" class="grid grid-cols-[1fr_1fr_auto] items-end gap-3">
              <label><span class="input-label">{{ t('accountAutoFast.minutes') }}</span><input v-model.number="stage.minutes" class="input" :data-testid="`auto-fast-minutes-${index}`" type="number" min="1" max="10080" step="1" required /></label>
              <label><span class="input-label">{{ t('accountAutoFast.usedBelow') }}</span><input v-model.number="stage.used_below_percent" class="input" :data-testid="`auto-fast-used-${index}`" type="number" min="0" max="100" step="0.1" required /></label>
              <button type="button" class="btn btn-secondary mb-0.5 px-2" :aria-label="t('accountAutoFast.removeStage', { index: index + 1 })" :data-testid="`auto-fast-remove-${index}`" :disabled="draft.stages.length <= 1" @click="draft.stages.splice(index, 1)"><Icon name="x" size="sm" /></button>
            </div>
            <button type="button" class="btn btn-secondary text-sm" data-testid="auto-fast-add" :disabled="draft.stages.length >= 12" @click="addStage">{{ t('accountAutoFast.addStage') }}</button>
          </section>
          <section class="grid gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:grid-cols-2">
            <label><span class="input-label">{{ t('accountAutoFast.finalMinutes') }}</span><input v-model.number="draft.final_minutes" class="input" data-testid="auto-fast-final" type="number" min="1" max="10079" step="1" required /><span class="mt-1 block text-xs text-gray-500">{{ t('accountAutoFast.finalHint') }}</span></label>
            <label><span class="input-label">{{ t('accountAutoFast.maxAge') }}</span><input v-model.number="draft.usage_max_age_seconds" class="input" data-testid="auto-fast-age" type="number" min="30" max="86400" step="1" required /><span class="mt-1 block text-xs text-gray-500">{{ t('accountAutoFast.maxAgeHint') }}</span></label>
          </section>
          <p class="text-xs text-gray-500">{{ t('accountAutoFast.restoreHint') }}</p>
          <p v-if="!valid" role="alert" class="text-sm text-amber-600">{{ t('accountAutoFast.invalid') }}</p>
          <div v-if="changedElsewhere" class="space-y-2 text-sm text-amber-600" role="alert">
            <p>{{ t('accountAutoFast.conflict') }}</p>
            <button type="button" class="btn btn-secondary text-xs" data-testid="auto-fast-reload" @click="load">{{ t('accountAutoFast.reload') }}</button>
          </div>
          <div class="flex items-center gap-3">
            <button v-admin-write type="submit" class="btn btn-primary" data-testid="auto-fast-save" :disabled="!valid || changedElsewhere">{{ t('common.save') }}</button>
            <span v-if="saved" role="status" class="text-sm text-green-600">{{ t('accountAutoFast.saved') }}</span>
          </div>
        </fieldset>
        <section class="space-y-3 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
          <div class="flex items-center justify-between gap-3"><h3 class="font-semibold">{{ t('accountAutoFast.status') }}</h3><button type="button" class="btn btn-secondary text-xs" data-testid="auto-fast-refresh" :disabled="refreshing || loading || saving" @click="refresh">{{ t('common.refresh') }}</button></div>
          <p>{{ view.config.enabled ? t('accountAutoFast.active') : t('accountAutoFast.disabled') }} · {{ t('accountAutoFast.total', { count: view.total }) }}</p>
          <p v-if="view.config.require_multiple_accounts && checked" data-testid="auto-fast-eligible-count" :class="view.eligible_accounts < 2 ? 'text-amber-600' : 'text-gray-500'">{{ t('accountAutoFast.eligibleAccounts', { count: view.eligible_accounts }) }} · {{ view.eligible_accounts < 2 ? t('accountAutoFast.waitingForAccounts') : t('accountAutoFast.enoughAccounts') }}</p>
          <p v-if="checked" class="text-xs text-gray-500">{{ t('accountAutoFast.checked') }} {{ formatTime(view.checked_at) }}</p>
          <p v-if="view.config.enabled && stale" class="text-amber-600">{{ t('accountAutoFast.stale') }}</p>
          <div v-for="account in view.accounts" :key="account.account_id" class="space-y-1 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
            <div class="flex flex-wrap justify-between gap-2"><span class="break-all font-medium">#{{ account.account_id }} {{ account.name }}</span><span :class="account.fast ? 'text-amber-600' : 'text-gray-500'">Fast {{ account.fast ? 'ON' : 'OFF' }} · {{ account.managed ? t('accountAutoFast.managed') : t('accountAutoFast.unmanaged') }}</span></div>
            <p class="text-xs text-gray-500">{{ sourceName(account.source) }}: {{ account.deadline ? formatTime(account.deadline) : '—' }} · 7D {{ account.used_percent === undefined ? t('accountAutoFast.unknown') : `${account.used_percent.toFixed(1)}%` }}</p>
            <p v-if="account.stage_minutes" class="text-xs text-gray-500">{{ t('accountAutoFast.currentStage', { minutes: account.stage_minutes }) }}</p>
            <p>{{ account.message }}</p>
          </div>
          <p v-if="view.total > view.accounts.length" class="text-xs text-gray-500">{{ t('accountAutoFast.truncated') }}</p>
        </section>
        <section v-if="view.events.length" class="space-y-2 text-sm">
          <h3 class="font-semibold">{{ t('accountAutoFast.history') }}</h3>
          <div v-for="(event, index) in view.events" :key="index" class="border-l-2 border-amber-400 pl-3"><p>{{ formatTime(event.at) }} · #{{ event.account_id }} · Fast {{ event.enabled ? 'ON' : 'OFF' }}</p><p class="text-gray-500">{{ event.reason }}</p></div>
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
import { accountAutoFastAPI, type AccountAutoFastConfig, type AccountAutoFastRule } from '@/api/admin/accountAutoFast'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ groups: { id: number; name: string; platform: string }[]; initialGroup?: string | number }>()
const emit = defineEmits<{ changed: [] }>()
const { t } = useI18n()
const show = ref(false), loading = ref(false), saving = ref(false), refreshing = ref(false), saved = ref(false)
const groupID = ref(0), draftRevision = ref(0), error = ref(''), now = ref(Date.now())
const view = ref<AccountAutoFastRule | null>(null), draft = ref<AccountAutoFastConfig | null>(null)
const openAIGroups = computed(() => props.groups.filter(g => g.platform === 'openai'))
const valid = computed(() => {
  const c = draft.value
  if (!c || c.stages.length < 1 || c.stages.length > 12) return false
  let previous = 10081
  for (const stage of c.stages) {
    if (!Number.isInteger(stage.minutes) || stage.minutes < 1 || stage.minutes >= previous ||
      typeof stage.used_below_percent !== 'number' || !Number.isFinite(stage.used_below_percent) || stage.used_below_percent < 0 || stage.used_below_percent > 100) return false
    previous = stage.minutes
  }
  return Number.isInteger(c.final_minutes) && c.final_minutes >= 1 && c.final_minutes < previous &&
    Number.isInteger(c.usage_max_age_seconds) && c.usage_max_age_seconds >= 30 && c.usage_max_age_seconds <= 86400
})
const checked = computed(() => !!view.value?.checked_at && Date.parse(view.value.checked_at) > 0)
const stale = computed(() => !checked.value || now.value - Date.parse(view.value!.checked_at) > 35000)
const changedElsewhere = computed(() => !!view.value && view.value.revision !== draftRevision.value)
let generation = 0, timer: ReturnType<typeof setInterval> | undefined
function cloneConfig(config: AccountAutoFastConfig): AccountAutoFastConfig { return { ...config, require_multiple_accounts: config.require_multiple_accounts ?? false, stages: config.stages.map(stage => ({ ...stage })) } }
function formatTime(value: string) { return new Date(value).toLocaleString() }
function sourceName(source: string) { return t(source === 'supplier_api' ? 'accountAutoFast.supplier' : source === 'import' ? 'accountAutoFast.imported' : 'accountAutoFast.local') }
function addStage() {
  if (!draft.value || draft.value.stages.length >= 12) return
  const last = draft.value.stages[draft.value.stages.length - 1]!
  draft.value.stages.push({ minutes: Math.max(draft.value.final_minutes + 1, Math.floor(last.minutes / 2)), used_below_percent: last.used_below_percent })
}
async function open() {
  show.value = true
  groupID.value = openAIGroups.value.some(g => g.id === Number(props.initialGroup)) ? Number(props.initialGroup) : (openAIGroups.value[0]?.id ?? 0)
  if (!timer) timer = setInterval(() => { now.value = Date.now(); void refresh() }, 10000)
  await load()
}
function close() {
  if (saving.value) return
  show.value = false; generation++
  if (timer) clearInterval(timer)
  timer = undefined
  emit('changed')
}
async function load() {
  const current = ++generation
  view.value = null; draft.value = null; error.value = ''; saved.value = false; loading.value = false
  if (!groupID.value) return
  loading.value = true
  try {
    const data = await accountAutoFastAPI.get(groupID.value)
    if (current === generation) { view.value = data; draft.value = cloneConfig(data.config); draftRevision.value = data.revision }
  } catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountAutoFast.loadError')) }
  finally { if (current === generation) loading.value = false }
}
async function refresh() {
  if (!show.value || !view.value || loading.value || saving.value || refreshing.value) return
  const current = generation
  refreshing.value = true
  try {
    const data = await accountAutoFastAPI.get(groupID.value)
    // Keep the unsaved draft and its original revision; show external edits explicitly.
    if (current === generation) { view.value = data; error.value = '' }
  } catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountAutoFast.loadError')) }
  finally { refreshing.value = false }
}
async function save() {
  if (!valid.value || !view.value || !draft.value || changedElsewhere.value || loading.value || saving.value) return
  const current = ++generation
  saving.value = true; error.value = ''; saved.value = false
  try {
    const data = await accountAutoFastAPI.save(groupID.value, draftRevision.value, cloneConfig(draft.value))
    if (current === generation) { view.value = data; draft.value = cloneConfig(data.config); draftRevision.value = data.revision; saved.value = true; emit('changed') }
  } catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountAutoFast.saveError')) }
  finally { saving.value = false }
}
onUnmounted(() => { generation++; if (timer) clearInterval(timer) })
</script>
