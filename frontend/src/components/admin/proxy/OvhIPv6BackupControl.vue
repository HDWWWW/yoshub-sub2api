<template>
  <div class="mt-3 space-y-2 border-t border-gray-200 pt-3 text-sm dark:border-dark-700">
    <div class="flex flex-wrap items-center gap-3">
      <label class="flex items-center gap-2">
        <input v-admin-write type="checkbox" role="switch" :checked="status?.enabled === true" :aria-checked="status?.enabled === true"
          :disabled="saving || !status || (!status.enabled && !status.available)" @click.prevent="toggle" />
        <span>{{ t('ovhIPv6.backupAuto') }}</span>
      </label>
      <code v-if="status?.prefix" class="text-xs">{{ status.prefix }}</code>
      <span v-if="status" class="text-xs">{{ status.available ? t('ovhIPv6.bound', { count: status.bound }) : t('ovhIPv6.unavailable') }}</span>
      <button type="button" class="btn btn-secondary ml-auto px-2 py-1 text-xs" :disabled="saving" @click="load">{{ t('ovhIPv6.refresh') }}</button>
      <button type="button" class="btn btn-secondary px-2 py-1 text-xs" :aria-expanded="visible" @click="toggleRecords">{{ t('ovhIPv6.records') }}</button>
    </div>
    <p v-if="error" role="alert" class="text-xs text-red-600">{{ error }}</p>
    <div v-if="visible" class="max-h-64 overflow-auto">
      <table v-if="rows.length" class="w-full text-left text-xs">
        <thead><tr><th>IPv6</th><th>{{ t('ovhIPv6.pool') }}</th><th>{{ t('ovhIPv6.retiredAt') }}</th></tr></thead>
        <tbody><tr v-for="r in rows" :key="r.id"><td class="break-all py-1 font-mono">{{ r.address }}</td><td>{{ t('ovhIPv6.' + r.pool) }}</td><td>{{ new Date(r.retired_at).toLocaleString() }}</td></tr></tbody>
      </table>
      <p v-else-if="!loadingRecords && !recordsError" class="text-xs">{{ t('ovhIPv6.emptyRecords') }}</p>
      <p v-if="recordsError" role="alert" class="text-xs text-red-600">{{ recordsError }}</p>
      <button v-if="more" type="button" class="btn btn-secondary my-1 px-2 py-1 text-xs" :disabled="loadingRecords" @click="readRecords">{{ t('ovhIPv6.moreRecords') }}</button>
    </div>
  </div>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ovhIPv6API, type OvhIPv6Status, type Retirement } from '@/api/admin/ovhIPv6'
import { extractApiErrorMessage } from '@/utils/apiError'
const { t } = useI18n()
const status = ref<OvhIPv6Status | null>(null)
const saving = ref(false), error = ref('')
const visible = ref(false), loadingRecords = ref(false), more = ref(false), recordsError = ref('')
const rows = ref<Retirement[]>([])
async function load() {
  if (saving.value) return
  saving.value = true
  try { status.value = await ovhIPv6API.getBackup(); error.value = '' }
  catch (cause) { error.value = extractApiErrorMessage(cause, t('ovhIPv6.loadFailed')) }
  finally { saving.value = false }
}
async function toggle() {
  if (!status.value || saving.value) return
  saving.value = true
  try { status.value = await ovhIPv6API.setBackup(!status.value.enabled); error.value = '' }
  catch (cause) { error.value = extractApiErrorMessage(cause, t('ovhIPv6.saveFailed')) }
  finally { saving.value = false }
}
async function readRecords() {
  if (loadingRecords.value) return
  loadingRecords.value = true
  try {
    const page = await ovhIPv6API.retirements(rows.value.at(-1)?.id ?? 0)
    rows.value.push(...page)
    more.value = page.length === 50
    recordsError.value = ''
  } catch (cause) { recordsError.value = extractApiErrorMessage(cause, t('ovhIPv6.loadFailed')); more.value = true }
  finally { loadingRecords.value = false }
}
async function toggleRecords() {
  if (loadingRecords.value) return
  visible.value = !visible.value
  if (visible.value) { rows.value = []; more.value = false; await readRecords() }
}
onMounted(load)
</script>
