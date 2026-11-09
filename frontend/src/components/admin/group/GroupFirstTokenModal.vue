<template>
  <BaseDialog :show="show" :title="t('admin.groups.firstTokenOptimization.title')" @close="close">
    <div v-if="group" class="space-y-5">
      <p class="font-medium text-gray-900 dark:text-gray-100">{{ group.name }}</p>
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <template v-else>
        <label class="flex items-center gap-3">
          <input v-model="smartWS" type="checkbox" :disabled="saving || failed" data-testid="group-smart-ws" />
          <span>{{ t('admin.groups.firstTokenOptimization.smartWS') }}</span>
        </label>
        <p class="text-sm text-gray-500">{{ t('admin.groups.firstTokenOptimization.smartHint') }}</p>
        <label class="flex items-center gap-3">
          <input v-model="earlyLimits" type="checkbox" :disabled="saving || failed" data-testid="group-early-rate-limits" />
          <span>{{ t('admin.groups.firstTokenOptimization.earlyLimits') }}</span>
        </label>
        <p class="text-sm leading-6 text-gray-500">{{ t('admin.groups.firstTokenOptimization.earlyHint') }}</p>
        <p v-if="failed" class="text-sm text-red-600" role="alert">{{ t('admin.groups.failedToLoad') }}</p>
      </template>
    </div>
    <template #footer>
      <button class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.cancel') }}</button>
      <button class="btn btn-primary" :disabled="saving || loading || failed" @click="save">{{ t('common.save') }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { apiClient } from '@/api/client'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import type { AdminGroup } from '@/types'

const props = defineProps<{ show: boolean; group: AdminGroup | null }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n()
const appStore = useAppStore()
const smartWS = ref(false)
const earlyLimits = ref(false)
const loading = ref(false)
const saving = ref(false)
const failed = ref(false)
let generation = 0
const close = () => { if (!saving.value) emit('close') }
watch(() => [props.show, props.group?.id] as const, async () => {
  const token = ++generation
  if (!props.show || !props.group) return
  loading.value = true
  failed.value = false
  smartWS.value = props.group.smart_ws_mode === 'enabled'
  try {
    const { data } = await apiClient.get<{ early_rate_limits_enabled: boolean }>(`/admin/settings/group-first-token-optimization/${props.group.id}`)
    if (token === generation) earlyLimits.value = data.early_rate_limits_enabled
  } catch { if (token === generation) failed.value = true }
  finally { if (token === generation) loading.value = false }
}, { immediate: true })

async function save() {
  if (!props.group || saving.value || failed.value) return
  saving.value = true
  try {
    const mode = smartWS.value ? 'enabled' : 'disabled'
    if ((props.group.smart_ws_mode === 'enabled') !== smartWS.value) {
      await adminAPI.groups.update(props.group.id, { smart_ws_mode: mode })
      emit('saved')
    }
    await apiClient.put(`/admin/settings/group-first-token-optimization/${props.group.id}`, { early_rate_limits_enabled: earlyLimits.value })
    appStore.showSuccess(t('admin.groups.rateSaved'))
    emit('saved')
    emit('close')
  } catch { appStore.showError(t('admin.groups.failedToSave')) }
  finally { saving.value = false }
}
</script>
