<template>
  <BaseDialog :show="show" :title="t('accountRefill.schedulingTitle')" width="extra-wide" @close="emit('close')">
    <div class="space-y-4">
      <p class="text-sm text-gray-500">{{ t('accountRefill.schedulingHint') }}</p>
      <div class="flex flex-wrap items-end gap-3">
        <label class="flex-1"><span class="input-label">{{ t('accountRefill.chooseGroup') }}</span>
          <select v-model.number="groupID" class="input" :disabled="saving !== null" @change="load">
            <option :value="0">{{ t('accountRefill.allOpenAI') }}</option>
            <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
          </select>
        </label>
        <button class="btn btn-secondary" :disabled="loading || saving !== null" @click="load">{{ t('common.refresh') }}</button>
        <button v-admin-write class="btn btn-secondary" :disabled="loading || saving !== null" data-testid="restore-all-scheduling" @click="restoreAll">{{ t('accountRefill.restoreAllAutomatic') }}</button>
      </div>
      <p class="text-xs text-gray-500">{{ t('accountRefill.restoreAllHint') }}</p>
      <p v-if="notice" class="text-sm text-green-700" role="status">{{ notice }}</p>
      <p class="text-xs text-gray-500">{{ t(expiryEnabled ? 'accountRefill.expirySortEnabled' : 'accountRefill.expirySortDisabled') }}</p>
      <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <div v-else class="max-h-[60vh] overflow-auto">
        <table class="w-full text-left text-sm">
          <thead class="sticky top-0 bg-gray-50 dark:bg-dark-800"><tr>
            <th class="p-2">{{ t('admin.accounts.columns.name') }}</th>
            <th class="p-2">{{ t('accountRefill.automaticPriority') }}</th>
            <th class="p-2">{{ t('accountRefill.effectivePriority') }}</th>
            <th class="p-2">{{ t('accountRefill.effectiveDeadline') }}</th>
            <th class="p-2">{{ t('accountRefill.manualPriority') }}</th>
          </tr></thead>
          <tbody><tr v-for="row in items" :key="row.account_id" class="border-t border-gray-100 dark:border-dark-700">
            <td class="p-2"><div class="max-w-64 truncate" :title="row.name">{{ row.name }}</div><span class="text-xs text-gray-400">#{{ row.account_id }}</span></td>
            <td class="p-2 font-mono">{{ row.automatic_priority }}</td>
            <td class="p-2"><span class="font-mono">{{ row.effective_priority }}</span><span class="ml-2 text-xs text-gray-500">{{ t(row.manual_priority === null ? 'accountRefill.priorityAuto' : 'accountRefill.priorityManual') }}</span></td>
            <td class="whitespace-nowrap p-2 text-xs">{{ row.effective_expires_at ? formatDateTime(row.effective_expires_at) : '—' }}</td>
            <td class="p-2"><div class="flex items-center gap-2">
              <input v-model.number="drafts[row.account_id]" type="number" min="0" max="9999" step="1" class="input w-20" :disabled="saving !== null" :aria-label="`${t('accountRefill.manualPriority')} ${row.name}`" />
              <button v-admin-write class="btn btn-secondary text-xs" :disabled="saving !== null || !valid(drafts[row.account_id])" @click="save(row.account_id, 'manual')">{{ t('common.save') }}</button>
              <button v-admin-write class="btn btn-secondary text-xs" :disabled="saving !== null || row.manual_priority === null" @click="save(row.account_id, 'auto')">{{ t('accountRefill.restoreAutomatic') }}</button>
            </div></td>
          </tr></tbody>
        </table>
        <p v-if="!items.length" class="p-5 text-center text-gray-500">{{ t('accountRefill.noSchedulingCandidates') }}</p>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { accountSchedulingAPI, type AccountSchedulingPreviewItem } from '@/api/admin/accountScheduling'
import { update, bulkUpdate } from '@/api/admin/accounts'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'
import type { AdminGroup } from '@/types'

const props = defineProps<{ show: boolean; groups: AdminGroup[]; initialGroup?: number }>()
const emit = defineEmits<{ close: []; updated: [] }>()
const { t } = useI18n()
const groupID = ref(0), loading = ref(false), expiryEnabled = ref(false), error = ref('')
const saving = ref<number | null>(null)
const notice = ref('')
const items = ref<AccountSchedulingPreviewItem[]>([]), drafts = ref<Record<number, number>>({})
let generation = 0
const valid = (value: number | undefined) => typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= 9999
watch(() => props.show, (show) => {
  generation++
  if (show) { notice.value = ''; groupID.value = props.initialGroup || 0; void load() }
})
async function load() {
  const current = ++generation
  loading.value = true; error.value = ''
  try {
    const platform = props.groups.find(g => g.id === groupID.value)?.platform || 'openai'
    const result = await accountSchedulingAPI.preview(groupID.value, platform)
    if (current !== generation) return
    items.value = result.items; expiryEnabled.value = result.expiry_priority_enabled
    drafts.value = Object.fromEntries(result.items.map(row => [row.account_id, row.effective_priority]))
  } catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRefill.schedulingError')) }
  finally { if (current === generation) loading.value = false }
}
async function save(id: number, mode: 'manual' | 'auto') {
  if (saving.value !== null || (mode === 'manual' && !valid(drafts.value[id]))) return
  saving.value = id; error.value = ''
  try {
    await update(id, { yoshub_priority_mode: mode, ...(mode === 'manual' ? { priority: drafts.value[id] } : {}) })
    emit('updated'); await load()
  } catch (e) { error.value = extractApiErrorMessage(e, t('accountRefill.schedulingError')) }
  finally { saving.value = null }
}
async function restoreAll() {
  if (saving.value !== null) return
  saving.value = -1; error.value = ''; notice.value = ''
  try {
    const result = await bulkUpdate({ filters: {}, yoshub_priority_mode: 'auto' })
    emit('updated'); await load()
    notice.value = t('accountRefill.restoredAll', { count: result.success })
    if (result.failed) error.value = t('accountRefill.restoreAllFailed', { count: result.failed })
  } catch (e) { error.value = extractApiErrorMessage(e, t('accountRefill.schedulingError')) }
  finally { saving.value = null }
}
</script>
