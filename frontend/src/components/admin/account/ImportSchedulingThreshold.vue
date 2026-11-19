<template>
  <section class="space-y-2 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
    <label for="openai-import-threshold" class="input-label">{{ t('accountRefill.importThreshold') }}</label>
    <div class="flex flex-wrap items-center gap-3">
      <input id="openai-import-threshold" v-model.number="percent" type="number" min="1" max="100" step="1"
        class="input w-28" :disabled="disabled || busy || saved === null" aria-describedby="openai-import-threshold-hint"
        @keydown.enter.prevent="save" />
      <button v-admin-write type="button" class="btn btn-secondary" :disabled="disabled || !canSave" @click="save">
        {{ t('accountRefill.saveImportThreshold') }}
      </button>
      <button v-if="error && saved === null" type="button" class="btn btn-secondary" :disabled="busy" @click="load">{{ t('common.tryAgain') }}</button>
      <span v-if="confirmed" role="status" class="text-sm text-emerald-700 dark:text-emerald-300">{{ t('accountRefill.importThresholdSaved') }}</span>
    </div>
    <p id="openai-import-threshold-hint" class="input-hint">{{ t('accountRefill.importThresholdHint') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { accountRefillAPI } from '@/api/admin/accountRefill'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ disabled: boolean }>()
const { t } = useI18n()
const saved = ref<number | null>(null)
const percent = ref<number | ''>('')
const busy = ref(false)
const error = ref('')
const justSaved = ref(false)
const confirmed = computed(() => justSaved.value && !busy.value && !error.value && percent.value === saved.value)
const canSave = computed(() => !busy.value && saved.value !== null && typeof percent.value === 'number' &&
  Number.isInteger(percent.value) && percent.value >= 1 && percent.value <= 100 && percent.value !== saved.value)

async function load() {
  busy.value = true
  error.value = ''
  try {
    saved.value = await accountRefillAPI.getImportThreshold()
    percent.value = saved.value
  } catch (e) {
    error.value = extractApiErrorMessage(e, t('accountRefill.loadError'))
  } finally {
    busy.value = false
  }
}

async function save() {
  if (props.disabled || !canSave.value || typeof percent.value !== 'number') return
  busy.value = true
  error.value = ''
  justSaved.value = false
  try {
    saved.value = await accountRefillAPI.setImportThreshold(percent.value)
    percent.value = saved.value
    justSaved.value = true
  } catch (e) {
    error.value = extractApiErrorMessage(e, t('accountRefill.error'))
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>
