<template>
  <div class="relative">
    <button type="button" role="switch" class="btn btn-secondary gap-2" data-testid="astra-priority-toggle"
      :aria-label="t('admin.accounts.astraPriority.title')" :aria-checked="enabled === true"
      :aria-busy="loading || saving" :disabled="enabled === null || loading || saving"
      :title="t('admin.accounts.astraPriority.hint')" @click="toggle">
      <span>{{ t('admin.accounts.astraPriority.title') }}</span>
      <span class="inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors" :class="enabled ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-500'">
        <span class="h-3.5 w-3.5 rounded-full bg-white transition-transform" :class="enabled ? 'translate-x-[18px]' : 'translate-x-1'" />
      </span>
    </button>
    <p v-if="error" class="mt-1 max-w-72 text-xs text-red-600 dark:text-red-400" role="alert">
      {{ error }} <button type="button" class="underline" :disabled="loading || saving" @click="load">{{ t('common.tryAgain') }}</button>
    </p>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { astraPriorityAPI } from '@/api/admin/astraPriority'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const enabled = ref<boolean | null>(null)
const loading = ref(false), saving = ref(false), error = ref('')
let disposed = false
let controller: AbortController | undefined

async function load() {
  if (loading.value || saving.value || disposed) return
  loading.value = true
  error.value = ''
  controller = new AbortController()
  try {
    const data = await astraPriorityAPI.get(controller.signal)
    if (!disposed) enabled.value = data.enabled
  } catch (cause) {
    if (!disposed) error.value = extractApiErrorMessage(cause, t('admin.accounts.astraPriority.loadFailed'))
  } finally { loading.value = false }
}

async function toggle() {
  if (enabled.value === null || loading.value || saving.value) return
  saving.value = true
  error.value = ''
  try {
    const data = await astraPriorityAPI.save(!enabled.value)
    if (!disposed) enabled.value = data.enabled
  } catch (cause) {
    if (!disposed) error.value = extractApiErrorMessage(cause, t('admin.accounts.astraPriority.saveFailed'))
  } finally { saving.value = false }
}

onMounted(load)
onUnmounted(() => { disposed = true; controller?.abort() })
</script>
