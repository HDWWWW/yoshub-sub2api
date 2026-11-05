<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
import { useAppStore } from '@/stores/app'
interface Images {
  Enabled: boolean; Mode: string; BaseURL: string; BodyLimitMiB: number; BudgetMiB: number; MaxRequests: number
  Policy: string; WarningRemaining: number; CompactReserve: number
  Limits: { MaxImageMiB: number; MaxImages: number; MaxTotalMiB: number; StorageMiB: number; StorageEntries: number; TTLMinutes: number }
}
const { t } = useI18n()
const app = useAppStore()
const settings = ref<{enabled: boolean; images: Images} | null>(null)
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const key = (name: string) => `admin.settings.excelBps.${name}`
const capacityFields = ['BodyLimitMiB', 'BudgetMiB', 'MaxRequests', 'WarningRemaining', 'CompactReserve'] as const
const limitFields = ['MaxImageMiB', 'MaxImages', 'MaxTotalMiB', 'StorageMiB', 'StorageEntries', 'TTLMinutes'] as const
async function load() {
  loading.value = true
  error.value = ''
  try { settings.value = (await apiClient.get('/admin/settings/excel-bps')).data }
  catch { error.value = t(key('loadFailed')) }
  finally { loading.value = false }
}
async function save() {
  saving.value = true
  error.value = ''
  try {
    await apiClient.put('/admin/settings/excel-bps', settings.value)
    app.showSuccess(t('common.saved'))
  } catch (e: any) { error.value = e.response?.data?.message || t(key('saveFailed')) }
  finally { saving.value = false }
}
onMounted(load)
</script>
<template>
  <section class="card mt-6 space-y-4 p-6" data-testid="excel-bps-settings">
    <h3 class="text-lg font-semibold">{{ t(key('title')) }}</h3>
    <p class="text-sm text-gray-500">{{ t(key('description')) }}</p>
    <p v-if="loading" class="text-sm">{{ t('common.loading') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }} <button v-if="!settings" type="button" class="btn btn-secondary" @click="load">{{ t('common.refresh') }}</button></p>
    <form v-if="settings" class="space-y-4" @submit.prevent="save">
      <label class="flex items-center gap-2"><input v-model="settings.enabled" type="checkbox" />{{ t(key('enabled')) }}</label>
      <label class="flex items-center gap-2"><input v-model="settings.images.Enabled" type="checkbox" />{{ t(key('imagesEnabled')) }}</label>
      <div v-if="settings.images.Enabled" class="space-y-4">
        <label class="block text-sm">{{ t(key('mode')) }}
          <select v-model="settings.images.Mode" class="input mt-1"><option value="native">{{ t(key('native')) }}</option><option value="relay">{{ t(key('relay')) }}</option></select>
        </label>
        <label v-if="settings.images.Mode === 'relay'" class="block text-sm">{{ t(key('origin')) }}<input v-model="settings.images.BaseURL" type="url" class="input mt-1" placeholder="https://sub2.example.com" required /></label>
        <p v-if="settings.images.Mode === 'relay'" class="text-xs text-gray-500">{{ t(key('relayHint')) }}</p>
        <label class="block text-sm">{{ t(key('policy')) }}<select v-model="settings.images.Policy" class="input mt-1"><option value="off">{{ t(key('off')) }}</option><option value="warn">{{ t(key('warn')) }}</option><option value="auto_compact">{{ t(key('autoCompact')) }}</option></select></label>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label v-for="field in capacityFields" :key="field" class="text-sm">{{ t(key(field)) }}<input v-model.number="settings.images[field]" type="number" min="1" step="1" required class="input mt-1" /></label>
          <label v-for="field in limitFields" :key="field" class="text-sm">{{ t(key(field)) }}<input v-model.number="settings.images.Limits[field]" type="number" min="1" step="1" required class="input mt-1" /></label>
        </div>
      </div>
      <button class="btn btn-primary" type="submit" :disabled="saving">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
    </form>
  </section>
</template>
