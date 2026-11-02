<template>
  <button type="button" class="btn btn-secondary" data-testid="open-retry-default" @click="open">{{ t('accountRefill.retryDefaultTitle') }}</button>
  <BaseDialog :show="show" :title="t('accountRefill.retryDefaultTitle')" width="normal" @close="!saving && (show = false)">
    <form class="space-y-4" @submit.prevent="save">
      <p class="text-sm text-gray-500">{{ t('accountRefill.retryDefaultHint') }}</p>
      <label class="block"><span class="input-label">{{ t('admin.accounts.poolModeRetryCount') }}</span><input v-model.number="count" data-testid="retry-default-count" class="input" type="number" min="0" :max="MAX_SAME_ACCOUNT_RETRY_COUNT" step="1" required :disabled="loading || saving" /></label>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <button type="submit" class="btn btn-primary" :disabled="loading || saving || !loaded || !valid">{{ t('common.save') }}</button>
    </form>
  </BaseDialog>
</template>
<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { accountRetryAPI } from '@/api/admin/accountRetry'
import { DEFAULT_SAME_ACCOUNT_RETRY_COUNT, MAX_SAME_ACCOUNT_RETRY_COUNT } from '@/components/account/sameAccountRetry'
import { extractApiErrorMessage } from '@/utils/apiError'
const { t } = useI18n()
const show = ref(false), loading = ref(false), saving = ref(false), loaded = ref(false), error = ref('')
const count = ref(DEFAULT_SAME_ACCOUNT_RETRY_COUNT)
const valid = computed(() => Number.isInteger(count.value) && count.value >= 0 && count.value <= MAX_SAME_ACCOUNT_RETRY_COUNT)
let generation = 0
async function open() {
  const current = ++generation
  show.value = true; loading.value = true; loaded.value = false; error.value = ''
  try { const data = await accountRetryAPI.get(); if (current === generation) { count.value = data.count; loaded.value = true } }
  catch (e) { if (current === generation) error.value = extractApiErrorMessage(e, t('accountRefill.retryDefaultError')) }
  finally { if (current === generation) loading.value = false }
}
async function save() {
  if (!valid.value || !loaded.value || loading.value || saving.value) return
  saving.value = true; error.value = ''
  try { await accountRetryAPI.save(count.value); show.value = false }
  catch (e) { error.value = extractApiErrorMessage(e, t('accountRefill.retryDefaultError')) }
  finally { saving.value = false }
}
</script>
