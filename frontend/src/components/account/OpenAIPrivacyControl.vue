<template>
  <div class="rounded-xl border border-gray-200 px-4 py-3 dark:border-dark-700">
    <label class="flex items-center justify-between gap-4 text-sm font-medium text-gray-900 dark:text-white">
      <span>{{ t('admin.accounts.privacyProtection') }}</span>
      <input type="checkbox" :checked="modelValue" :disabled="disabled" data-testid="openai-privacy-enabled"
        class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
        @change="$emit('update:modelValue', ($event.target as HTMLInputElement).checked)" />
    </label>
    <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t(modelValue ? 'admin.accounts.privacyProtectionOnHint' : 'admin.accounts.privacyProtectionOffHint') }}</p>
    <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('admin.accounts.privacyResultHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
defineProps<{ modelValue: boolean; disabled?: boolean }>()
defineEmits<{ 'update:modelValue': [value: boolean] }>()
const { t } = useI18n()
</script>
