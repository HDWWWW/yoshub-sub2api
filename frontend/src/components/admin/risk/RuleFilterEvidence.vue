<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { RuleFilterEvidence } from '@/api/admin/riskControl'
import ModerationHighlightedText from './ModerationHighlightedText.vue'
defineProps<{ evidence: RuleFilterEvidence }>()
const { t } = useI18n()
</script>

<template>
  <div class="space-y-3 rounded-lg border border-amber-200 p-4 dark:border-amber-800" data-test="rule-filter-evidence">
    <p class="font-semibold">{{ t('admin.riskControl.ruleFilter.result') }}: {{ evidence.action }} · {{ t('admin.riskControl.ruleFilter.score') }} {{ evidence.score }} / {{ evidence.threshold }}</p>
    <p class="text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.scoreHint') }} · {{ t('admin.riskControl.ruleFilter.rawScore') }} {{ evidence.raw_score }} · {{ t('admin.riskControl.ruleFilter.auditScore') }} {{ evidence.audit_score }}</p>
    <p v-if="evidence.reason" class="break-words text-sm">{{ evidence.reason }}</p>
    <p v-if="evidence.deferred" class="text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.deferred') }}</p>
    <p v-if="evidence.truncated" class="text-xs text-amber-700">{{ t('admin.riskControl.ruleFilter.truncated') }}</p>
    <p v-if="!evidence.signals.length" class="text-sm">{{ t('admin.riskControl.ruleFilter.noMatch') }}</p>
    <div v-for="(signal, index) in evidence.signals" :key="index" class="space-y-2 rounded bg-amber-50 p-3 dark:bg-amber-900/20">
      <p class="text-xs">{{ signal.origin }} · {{ signal.layer_mode }} · {{ signal.action }} · {{ t('admin.riskControl.ruleFilter.score') }} {{ signal.score }}</p>
      <p class="break-words text-sm">{{ signal.matches.map(match => `${match.name} (${match.category || 'custom'}, ${match.weight})`).join(' · ') }}</p>
      <ModerationHighlightedText v-if="signal.context" :text="signal.context" :keywords="signal.keywords" class="block max-h-64 overflow-auto whitespace-pre-wrap break-words text-sm" />
      <p v-else class="text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.contextUnavailable') }}</p>
    </div>
    <p class="break-all text-xs text-gray-400">{{ evidence.version }}</p>
  </div>
</template>
