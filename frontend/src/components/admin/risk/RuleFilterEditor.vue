<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import RuleFilterEvidenceView from './RuleFilterEvidence.vue'
import { getRuleFilterCatalog, testRuleFilter } from '@/api/admin/riskControl'
import type { RuleFilterConfig, RulePattern, RuleFilterCatalog, RuleFilterEvidence } from '@/api/admin/riskControl'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ modelValue: RuleFilterConfig }>()
const emit = defineEmits<{ 'update:modelValue': [value: RuleFilterConfig] }>()
const { t } = useI18n()
const catalog = ref<RuleFilterCatalog | null>(null)
const search = ref('')
const category = ref('')
const testText = ref('')
const result = ref<RuleFilterEvidence | null>(null)
const error = ref('')
const busy = ref(false)
const importInput = ref<HTMLInputElement | null>(null)
let revision = 0
watch(() => [props.modelValue, testText.value], () => { revision++; result.value = null }, { deep: true })
const categories = computed(() => [...new Set(catalog.value?.presets.map(r => r.category || 'custom') ?? [])].sort())
const presets = computed(() => (catalog.value?.presets ?? []).filter(r => (!category.value || r.category === category.value) && `${r.name} ${r.category} ${r.pattern}`.toLowerCase().includes(search.value.toLowerCase())))
const fields = ['all_patterns', 'any_patterns', 'exclude_patterns', 'authorization_exclude_patterns'] as const
const field = (event: Event) => (event.target as HTMLInputElement).value
const change = (patch: Partial<RuleFilterConfig>) => emit('update:modelValue', { ...props.modelValue, ...patch })
function updateRule(index: number, patch: Partial<RulePattern>) {
  const oldName = props.modelValue.custom_patterns[index].name
  const disabled = props.modelValue.disabled_patterns.filter(n => patch.enabled === undefined || n !== oldName).map(n => n === oldName && patch.name ? patch.name : n)
  change({ custom_patterns: props.modelValue.custom_patterns.map((r, i) => i === index ? { ...r, ...patch } : r), disabled_patterns: disabled })
}
function togglePreset(name: string, enabled: boolean) {
  change({ disabled_patterns: enabled ? props.modelValue.disabled_patterns.filter(n => n !== name) : [...new Set([...props.modelValue.disabled_patterns, name])] })
}
function addRule() {
  change({ custom_patterns: [...props.modelValue.custom_patterns, { name: `custom-${crypto.randomUUID().slice(0, 8)}`, pattern: '', category: 'custom', weight: 60, enabled: true, strict: false, signal_only: false }] })
}
function removeRule(index: number) {
  const name = props.modelValue.custom_patterns[index].name
  change({ custom_patterns: props.modelValue.custom_patterns.filter((_, i) => i !== index), disabled_patterns: props.modelValue.disabled_patterns.filter(n => n !== name) })
}
async function loadCatalog() {
  error.value = ''
  try { catalog.value = await getRuleFilterCatalog() } catch (err) { error.value = extractApiErrorMessage(err, t('admin.riskControl.ruleFilter.failed')) }
}
async function preview() {
  error.value = ''; busy.value = true
  const current = revision
  try { const value = await testRuleFilter(props.modelValue, testText.value); if (current === revision) result.value = value } catch (err) { error.value = extractApiErrorMessage(err, t('admin.riskControl.ruleFilter.failed')) } finally { busy.value = false }
}
async function importRules(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  busy.value = true; error.value = ''
  const current = revision
  try {
    if (file.size > 1024 * 1024) throw new Error(t('admin.riskControl.ruleFilter.invalidImport'))
    const parsed = JSON.parse(await file.text())
    const value: RuleFilterConfig = parsed.rule_filter ?? parsed
    if (!Array.isArray(value.custom_patterns) || !Array.isArray(value.disabled_patterns)) throw new Error(t('admin.riskControl.ruleFilter.invalidImport'))
    await testRuleFilter(value, '')
    if (current !== revision) throw new Error(t('admin.riskControl.ruleFilter.changed'))
    emit('update:modelValue', value)
  } catch (err) { error.value = extractApiErrorMessage(err, err instanceof Error ? err.message : t('admin.riskControl.ruleFilter.invalidImport')) } finally { busy.value = false; input.value = '' }
}
function exportRules() {
  const url = URL.createObjectURL(new Blob([JSON.stringify({ version: catalog.value?.version, rule_filter: props.modelValue }, null, 2)], { type: 'application/json' }))
  const link = document.createElement('a'); link.href = url; link.download = 'codex2api-local-rules.json'; link.click(); URL.revokeObjectURL(url)
}
function restoreDefaults() {
  if (!catalog.value) return
  // Restore upstream thresholds and all builtins while retaining custom rules.
  change({ ...catalog.value.defaults, enabled: props.modelValue.enabled, custom_patterns: props.modelValue.custom_patterns })
}
onMounted(loadCatalog)
</script>

<template>
  <div class="space-y-5" data-test="rule-filter-editor">
    <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('admin.riskControl.ruleFilter.hint') }}</p>
    <div class="flex flex-wrap items-center gap-3">
      <Toggle :model-value="modelValue.enabled" :aria-label="t('admin.riskControl.ruleFilter.enabled')" data-test="rule-filter-enabled" @update:model-value="change({ enabled: $event })" />
      <span class="text-sm font-semibold">{{ t('admin.riskControl.ruleFilter.enabled') }}</span>
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="importInput?.click()">{{ t('admin.riskControl.ruleFilter.import') }}</button>
      <button type="button" class="btn btn-secondary" @click="exportRules">{{ t('admin.riskControl.ruleFilter.export') }}</button>
      <button type="button" class="btn btn-secondary" :disabled="!catalog || busy" data-test="restore-presets" @click="restoreDefaults">{{ t('admin.riskControl.ruleFilter.restore') }}</button>
      <input ref="importInput" type="file" accept=".json,application/json" class="hidden" @change="importRules" />
    </div>
    <div class="grid gap-4 sm:grid-cols-3">
      <label><span class="input-label">{{ t('admin.riskControl.ruleFilter.mode') }}</span><select :value="modelValue.mode" class="input" @change="change({ mode: field($event) as RuleFilterConfig['mode'] })"><option value="block">{{ t('admin.riskControl.ruleFilter.block') }}</option><option value="monitor">{{ t('admin.riskControl.ruleFilter.monitor') }}</option><option value="warn">{{ t('admin.riskControl.ruleFilter.warn') }}</option></select></label>
      <label><span class="input-label">{{ t('admin.riskControl.ruleFilter.threshold') }}</span><input :value="modelValue.threshold" type="number" min="1" max="500" class="input" data-test="rule-threshold" @input="change({ threshold: Number(field($event)) })" /></label>
      <label><span class="input-label">{{ t('admin.riskControl.ruleFilter.strictThreshold') }}</span><input :value="modelValue.strict_threshold" type="number" :min="modelValue.threshold" max="1000" class="input" @input="change({ strict_threshold: Number(field($event)) })" /></label>
    </div>
    <label class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="modelValue.strict_terminal_enabled" @change="change({ strict_terminal_enabled: ($event.target as HTMLInputElement).checked })" />{{ t('admin.riskControl.ruleFilter.strictTerminal') }}</label>
    <p class="text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.scoreHint') }}</p>
    <section class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
      <p class="font-semibold">{{ t('admin.riskControl.ruleFilter.presets') }} · {{ catalog?.presets.length ?? 0 }}</p>
      <p v-if="!catalog" class="text-sm text-gray-500">{{ t('common.loading') }} <button type="button" class="underline" @click="loadCatalog">{{ t('common.tryAgain') }}</button></p>
      <div class="grid gap-3 sm:grid-cols-2"><input v-model="search" :placeholder="t('admin.riskControl.ruleFilter.search')" :aria-label="t('admin.riskControl.ruleFilter.search')" class="input" /><select v-model="category" class="input" :aria-label="t('admin.riskControl.ruleFilter.category')"><option value="">{{ t('admin.riskControl.ruleFilter.allCategories') }}</option><option v-for="item in categories" :key="item" :value="item">{{ item }}</option></select></div>
      <div class="max-h-96 space-y-2 overflow-auto">
        <div v-for="rule in presets" :key="rule.name" class="rounded border border-gray-100 p-3 dark:border-dark-700" data-test="preset-rule">
          <div class="flex items-start justify-between gap-3"><details class="min-w-0 flex-1"><summary class="cursor-pointer break-words text-sm">{{ rule.name }} · {{ rule.category }} · {{ rule.weight }}</summary><pre class="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all text-xs">{{ JSON.stringify(rule, null, 2) }}</pre></details><Toggle :model-value="!modelValue.disabled_patterns.includes(rule.name)" :aria-label="rule.name" @update:model-value="togglePreset(rule.name, $event)" /></div>
        </div>
      </div>
      <p class="break-all text-xs text-gray-400">{{ catalog?.version }}</p>
    </section>
    <div class="flex items-center justify-between"><p class="font-semibold">{{ t('admin.riskControl.ruleFilter.custom') }} · {{ modelValue.custom_patterns.length }} / 100</p><button type="button" class="btn btn-primary" :disabled="modelValue.custom_patterns.length >= 100" data-test="add-rule" @click="addRule">{{ t('admin.riskControl.ruleFilter.add') }}</button></div>
    <section v-for="(rule, index) in modelValue.custom_patterns" :key="index" class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600" data-test="custom-rule">
      <div class="flex items-center justify-between gap-3"><span class="break-words font-semibold">{{ rule.name }}</span><div class="flex items-center gap-3"><Toggle :model-value="rule.enabled !== false && !modelValue.disabled_patterns.includes(rule.name)" :aria-label="t('admin.riskControl.ruleFilter.enabled')" @update:model-value="updateRule(index, { enabled: $event })" /><button type="button" class="text-sm text-red-600" @click="removeRule(index)">{{ t('common.delete') }}</button></div></div>
      <div class="grid gap-3 sm:grid-cols-3"><label><span class="input-label">{{ t('admin.riskControl.ruleFilter.name') }}</span><input :value="rule.name" maxlength="100" class="input" data-test="rule-name" @input="updateRule(index, { name: field($event) })" /></label><label><span class="input-label">{{ t('admin.riskControl.ruleFilter.category') }}</span><input :value="rule.category" class="input" @input="updateRule(index, { category: field($event) })" /></label><label><span class="input-label">{{ t('admin.riskControl.ruleFilter.weight') }}</span><input :value="rule.weight" type="number" min="1" max="1000" class="input" @input="updateRule(index, { weight: Number(field($event)) })" /></label></div>
      <label class="block"><span class="input-label">{{ t('admin.riskControl.ruleFilter.pattern') }}</span><textarea :value="rule.pattern" rows="2" class="input font-mono text-sm" data-test="rule-pattern" @input="updateRule(index, { pattern: field($event) })"></textarea></label>
      <div class="flex flex-wrap gap-4 text-sm"><label><input type="checkbox" :checked="rule.strict" @change="updateRule(index, { strict: ($event.target as HTMLInputElement).checked })" /> {{ t('admin.riskControl.ruleFilter.strict') }}</label><label><input type="checkbox" :checked="rule.signal_only" @change="updateRule(index, { signal_only: ($event.target as HTMLInputElement).checked })" /> {{ t('admin.riskControl.ruleFilter.signalOnly') }}</label></div>
      <details><summary class="cursor-pointer text-sm">{{ t('admin.riskControl.ruleFilter.conditions') }}</summary><p class="my-2 text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.conditionsHint') }}</p><div class="grid gap-3 sm:grid-cols-2"><label v-for="key in fields" :key="key"><span class="input-label">{{ t(`admin.riskControl.ruleFilter.${key}`) }}</span><textarea :value="(rule[key] ?? []).join('\n')" rows="3" class="input font-mono text-xs" @input="updateRule(index, { [key]: field($event).split(/\r?\n/).filter(Boolean) })"></textarea></label></div><label class="mt-3 block"><span class="input-label">{{ t('admin.riskControl.ruleFilter.minMatches') }}</span><input :value="rule.min_matches ?? 0" type="number" min="0" :max="rule.any_patterns?.length ?? 0" class="input" @input="updateRule(index, { min_matches: Number(field($event)) })" /></label></details>
    </section>
    <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600"><label class="block"><span class="input-label">{{ t('admin.riskControl.ruleFilter.test') }}</span><textarea v-model="testText" rows="4" maxlength="100000" class="input" data-test="rule-test-text"></textarea></label><p class="text-xs text-gray-500">{{ t('admin.riskControl.ruleFilter.testHint') }}</p><button type="button" class="btn btn-secondary" :disabled="busy" data-test="test-rules" @click="preview">{{ t('admin.riskControl.ruleFilter.runTest') }}</button><RuleFilterEvidenceView v-if="result" :evidence="result" /></div>
    <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
  </div>
</template>
