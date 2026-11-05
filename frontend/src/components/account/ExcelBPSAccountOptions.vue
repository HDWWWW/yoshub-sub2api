<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { ExcelBPSAccountOptions } from '@/utils/excelBps'
const model = defineModel<ExcelBPSAccountOptions>({ required: true })
const { t } = useI18n()
</script>
<template>
  <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600" data-testid="excel-bps-account-options">
    <label class="flex items-center gap-2"><input v-model="model.enabled" type="checkbox" />{{ t('admin.accounts.openai.excelEnabled') }}</label>
    <p class="text-xs text-gray-500">{{ t('admin.accounts.openai.excelDescription') }}</p>
    <template v-if="model.enabled">
      <label class="block text-sm">{{ t('admin.accounts.openai.excelModels') }}<textarea v-model="model.models" class="input mt-1" rows="2" /></label>
      <p class="text-xs text-gray-500">{{ t('admin.accounts.openai.excelModelsHint') }}</p>
      <label class="flex items-center gap-2 text-sm"><input v-model="model.omitUnsupportedTools" type="checkbox" />{{ t('admin.accounts.openai.excelOmitTools') }}</label>
      <label class="flex items-center gap-2 text-sm"><input v-model="model.ignoreEncryptedContent" type="checkbox" />{{ t('admin.accounts.openai.excelIgnoreEncrypted') }}</label>
      <label class="flex items-center gap-2 text-sm"><input v-model="model.cacheCreationAsInput" type="checkbox" />{{ t('admin.accounts.openai.excelCacheInput') }}</label>
    </template>
  </div>
</template>
