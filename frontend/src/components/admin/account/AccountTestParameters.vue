<template>
  <div class="space-y-3">
    <div v-if="supportsReasoning">
      <label :for="`${idPrefix}-effort`" class="input-label mb-1.5 block">
        {{ t('accountTestParams.reasoningEffort') }}
      </label>
      <select
        :id="`${idPrefix}-effort`"
        class="input w-full"
        :value="reasoningEffort"
        :disabled="disabled"
        @change="emit('update:reasoningEffort', ($event.target as HTMLSelectElement).value)"
      >
        <option value="">{{ t('accountTestParams.upstreamDefault') }}</option>
        <option v-for="effort in efforts" :key="effort" :value="effort">{{ effort }}</option>
      </select>
      <p class="input-hint mt-1.5">{{ t('accountTestParams.reasoningHint') }}</p>
    </div>
    <TextArea
      :id="`${idPrefix}-prompt`"
      :model-value="prompt"
      :label="t('accountTestParams.prompt')"
      :placeholder="t('accountTestParams.promptPlaceholder')"
      :hint="t('accountTestParams.promptHint')"
      :disabled="disabled"
      :rows="3"
      @update:model-value="emit('update:prompt', $event)"
    />
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import TextArea from '@/components/common/TextArea.vue'

withDefaults(defineProps<{
  prompt: string
  reasoningEffort: string
  idPrefix: string
  disabled?: boolean
  supportsReasoning?: boolean
}>(), { disabled: false, supportsReasoning: true })

const emit = defineEmits<{
  (e: 'update:prompt', value: string): void
  (e: 'update:reasoningEffort', value: string): void
}>()
const { t } = useI18n()
const efforts = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']
</script>
