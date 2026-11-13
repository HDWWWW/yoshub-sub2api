<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.bulkConcurrency.title')"
    width="normal"
    :close-on-escape="!submitting"
    :show-close-button="!submitting"
    @close="handleClose"
  >
    <form v-admin-read-only-form id="bulk-concurrency-form" class="space-y-5" @submit.prevent="submit">
      <div class="rounded-lg bg-gray-50 p-4 dark:bg-dark-800">
        <p class="text-sm font-medium text-gray-900 dark:text-white" data-testid="concurrency-group">{{ groupLabel }}</p>
        <p class="mt-1 text-sm text-gray-600 dark:text-dark-300" data-testid="concurrency-scope">
          {{ loading ? t('common.loading') : t('admin.accounts.bulkConcurrency.scope', { count }) }}
        </p>
        <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.accounts.bulkConcurrency.scopeHint') }}</p>
      </div>
      <div>
        <label for="bulk-concurrency-value" class="input-label">{{ t('admin.accounts.concurrency') }}</label>
        <input
          id="bulk-concurrency-value"
          v-model.number="concurrency"
          type="number"
          min="1"
          max="2147483647"
          step="1"
          required
          class="input"
          :disabled="submitting"
          data-testid="bulk-concurrency-value"
        />
        <p class="input-hint">{{ t('admin.accounts.bulkConcurrency.sharedHint') }}</p>
      </div>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    </form>
    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="submitting" @click="handleClose">{{ t('common.cancel') }}</button>
      <button v-admin-write
        type="submit"
        form="bulk-concurrency-form"
        class="btn btn-primary"
        :disabled="loading || submitting || count === 0 || !validConcurrency || !validGroup"
        data-testid="apply-bulk-concurrency"
      >
        {{ submitting ? t('common.saving') : t('admin.accounts.bulkConcurrency.apply') }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'

type Filters = {
  platform?: string
  type?: string
  status?: string
  group?: string
  search?: string
  privacy_mode?: string
}

const props = defineProps<{ show: boolean; filters: Filters; groupLabel: string }>()
const emit = defineEmits<{ close: []; updated: [] }>()
const { t } = useI18n()
const appStore = useAppStore()
const concurrency = ref<number | string>(50)
const count = ref(0)
const loading = ref(false)
const submitting = ref(false)
const error = ref('')
const snapshot = ref<Filters>({})
let previewVersion = 0

const validGroup = computed(() => snapshot.value.group === 'ungrouped' || /^\d+$/.test(snapshot.value.group ?? '') && Number(snapshot.value.group) > 0)
const validConcurrency = computed(() => typeof concurrency.value === 'number' && Number.isInteger(concurrency.value) && concurrency.value > 0 && concurrency.value <= 2147483647)

watch(() => props.show, async (show) => {
  const version = ++previewVersion
  loading.value = false
  if (!show) return
  snapshot.value = { ...props.filters }
  count.value = 0
  concurrency.value = 50
  error.value = ''
  if (!validGroup.value) {
    error.value = t('admin.accounts.bulkConcurrency.chooseGroup')
    return
  }
  loading.value = true
  try {
    const preview = await adminAPI.accounts.list(1, 1, snapshot.value)
    if (version === previewVersion) count.value = preview.total
  } catch {
    if (version === previewVersion) error.value = t('admin.accounts.bulkConcurrency.previewFailed')
  } finally {
    if (version === previewVersion) loading.value = false
  }
}, { immediate: true })

const handleClose = () => {
  if (!submitting.value) emit('close')
}

const submit = async () => {
  if (loading.value || submitting.value || count.value === 0 || !validGroup.value || !validConcurrency.value) return
  submitting.value = true
  error.value = ''
  try {
    const result = await adminAPI.accounts.bulkUpdate({ filters: { ...snapshot.value }, concurrency: concurrency.value })
    if (result.failed > 0) {
      error.value = t('admin.accounts.bulkConcurrency.partial', { success: result.success, failed: result.failed })
      return
    }
    appStore.showSuccess(t('admin.accounts.bulkConcurrency.success', { count: result.success, value: concurrency.value }))
    emit('updated')
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : t('admin.accounts.bulkConcurrency.failed')
  } finally {
    submitting.value = false
  }
}
</script>
