<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { testCombinationRules } from '@/api/admin/riskControl'
import type { CombinationRule, CombinationRuleHit } from '@/api/admin/riskControl'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ modelValue: CombinationRule[] }>()
const emit = defineEmits<{ 'update:modelValue': [rules: CombinationRule[]] }>()
const { t } = useI18n()
const testText = ref('')
const matches = ref<CombinationRuleHit[] | null>(null)
const busy = ref(false)
const error = ref('')
const importInput = ref<HTMLInputElement | null>(null)
let revision = 0
watch(() => [props.modelValue, testText.value], () => { revision++; matches.value = null }, { deep: true })

function update(index: number, patch: Partial<CombinationRule>) {
  emit('update:modelValue', props.modelValue.map((rule, i) => i === index ? { ...rule, ...patch } : rule))
}
function addRule() {
  emit('update:modelValue', [...props.modelValue, {
    id: crypto.randomUUID(), name: '', enabled: true, action: 'observe', scope: 'conversation', window_characters: 400,
    groups: [{ name: '', keywords: [] }, { name: '', keywords: [] }],
  }])
}
function updateGroup(index: number, groupIndex: number, patch: Partial<CombinationRule['groups'][number]>) {
  update(index, { groups: props.modelValue[index].groups.map((group, i) => i === groupIndex ? { ...group, ...patch } : group) })
}
function field(event: Event): string { return (event.target as HTMLInputElement).value }
async function preview() {
  busy.value = true
  error.value = ''
  const currentRevision = revision
  try {
    const result = await testCombinationRules(props.modelValue, testText.value)
    if (currentRevision === revision) matches.value = result.matches
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.riskControl.combination.failed'))
  } finally { busy.value = false }
}
async function importRules(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  busy.value = true
  error.value = ''
  try {
    if (file.size > 2 * 1024 * 1024) throw new Error(t('admin.riskControl.combination.invalidImport'))
    const parsed: unknown = JSON.parse(await file.text())
    const rules = Array.isArray(parsed) ? parsed : (parsed as { combination_rules?: unknown })?.combination_rules
    if (!Array.isArray(rules)) throw new Error(t('admin.riskControl.combination.invalidImport'))
    // Use server validation for the same limits and schema used when saving.
    const combined = [...props.modelValue, ...rules] as CombinationRule[]
    const importRevision = revision
    await testCombinationRules(combined, '')
    if (importRevision !== revision) throw new Error(t('admin.riskControl.combination.changed'))
    emit('update:modelValue', combined)
  } catch (err) {
    error.value = extractApiErrorMessage(err, err instanceof Error ? err.message : t('admin.riskControl.combination.invalidImport'))
  } finally { input.value = ''; busy.value = false }
}
function exportRules() {
  const url = URL.createObjectURL(new Blob([JSON.stringify({ combination_rules: props.modelValue }, null, 2)], { type: 'application/json' }))
  const link = document.createElement('a')
  link.href = url
  link.download = 'combination-rules.json'
  link.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <div class="space-y-5" data-test="combination-editor">
    <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('admin.riskControl.combination.hint') }}</p>
    <div class="flex flex-wrap items-center gap-2">
      <button type="button" class="btn btn-primary" :disabled="modelValue.length >= 100 || busy" data-test="add-rule" @click="addRule">{{ t('admin.riskControl.combination.add') }}</button>
      <button v-admin-write type="button" class="btn btn-secondary" :disabled="busy" @click="importInput?.click()">{{ t('admin.riskControl.combination.import') }}</button>
      <button v-admin-write type="button" class="btn btn-secondary" :disabled="!modelValue.length" @click="exportRules">{{ t('admin.riskControl.combination.export') }}</button>
      <input ref="importInput" type="file" accept=".json,application/json" class="hidden" @change="importRules" />
      <span class="text-sm text-gray-500">{{ modelValue.length }} / 100</span>
    </div>
    <p v-if="!modelValue.length" class="rounded-lg border border-dashed p-6 text-center text-sm text-gray-500 dark:border-dark-600">{{ t('admin.riskControl.combination.empty') }}</p>
    <section v-for="(rule, index) in modelValue" :key="rule.id" class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600" data-test="combination-rule">
      <div class="flex items-center justify-between gap-3">
        <span class="min-w-0 break-words font-semibold">{{ rule.name || t('admin.riskControl.combination.unnamed') }}</span>
        <div class="flex shrink-0 items-center gap-3">
          <Toggle v-admin-read-only-control :model-value="rule.enabled" :aria-label="t('admin.riskControl.combination.enabled')" @update:model-value="update(index, { enabled: $event })" />
          <button v-admin-write type="button" class="text-sm text-red-600 hover:underline" data-test="remove-rule" @click="emit('update:modelValue', modelValue.filter((_, i) => i !== index))">{{ t('common.delete') }}</button>
        </div>
      </div>
      <div class="grid gap-4 sm:grid-cols-2">
        <label class="block"><span class="input-label">{{ t('admin.riskControl.combination.name') }}</span><input :value="rule.name" maxlength="120" class="input" data-test="rule-name" @input="update(index, { name: field($event) })" /></label>
        <label class="block"><span class="input-label">{{ t('admin.riskControl.combination.action') }}</span><select :value="rule.action" class="input" data-test="rule-action" @change="update(index, { action: field($event) as CombinationRule['action'] })"><option value="observe">{{ t('admin.riskControl.combination.observe') }}</option><option value="block">{{ t('admin.riskControl.combination.block') }}</option></select></label>
        <label class="block"><span class="input-label">{{ t('admin.riskControl.combination.window') }}</span><input :value="rule.window_characters" type="number" min="1" max="12000" step="1" class="input" data-test="rule-window" @input="update(index, { window_characters: Number(field($event)) })" /></label>
        <label class="block"><span class="input-label">{{ t('admin.riskControl.combination.scope') }}</span><select :value="rule.scope" class="input" data-test="rule-scope" @change="update(index, { scope: field($event) as CombinationRule['scope'] })"><option value="conversation">{{ t('admin.riskControl.combination.conversation') }}</option><option value="current_user">{{ t('admin.riskControl.combination.current') }}</option></select></label>
      </div>
      <div class="grid gap-3 sm:grid-cols-2">
        <div v-for="(group, groupIndex) in rule.groups" :key="groupIndex" class="space-y-2 rounded-lg bg-gray-50 p-3 dark:bg-dark-800" data-test="rule-group">
          <div class="flex items-center justify-between"><span class="text-xs text-gray-500">{{ t('admin.riskControl.combination.group', { n: groupIndex + 1 }) }}</span><button v-admin-write type="button" class="text-xs text-red-600 disabled:opacity-40" :disabled="rule.groups.length <= 2" @click="update(index, { groups: rule.groups.filter((_, i) => i !== groupIndex) })">{{ t('common.delete') }}</button></div>
          <label class="block"><span class="sr-only">{{ t('admin.riskControl.combination.groupName') }}</span><input :value="group.name" :placeholder="t('admin.riskControl.combination.groupName')" maxlength="80" class="input" data-test="group-name" @input="updateGroup(index, groupIndex, { name: field($event) })" /></label>
          <label class="block"><span class="sr-only">{{ t('admin.riskControl.combination.words') }}</span><textarea :value="group.keywords.join('\n')" :placeholder="t('admin.riskControl.combination.words')" rows="5" class="input resize-y text-sm" data-test="group-words" @input="updateGroup(index, groupIndex, { keywords: field($event).split(/\r?\n/) })"></textarea></label>
        </div>
      </div>
      <button v-admin-write type="button" class="btn btn-secondary text-sm" :disabled="rule.groups.length >= 8" data-test="add-group" @click="update(index, { groups: [...rule.groups, { name: '', keywords: [] }] })">{{ t('admin.riskControl.combination.addGroup') }}</button>
      <p class="break-all text-xs text-gray-400">ID: {{ rule.id }}</p>
    </section>
    <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
      <label class="block"><span class="input-label">{{ t('admin.riskControl.combination.test') }}</span><textarea v-model="testText" rows="4" maxlength="100000" class="input resize-y" data-test="rule-test-text"></textarea></label>
      <p class="text-xs text-gray-500">{{ t('admin.riskControl.combination.testHint') }}</p>
      <button type="button" class="btn btn-secondary" :disabled="busy || !modelValue.length" data-test="test-rules" @click="preview">{{ t('admin.riskControl.combination.runTest') }}</button>
      <div v-if="matches !== null" class="space-y-2 text-sm" role="status" data-test="rule-test-result">
        <p v-if="!matches.length">{{ t('admin.riskControl.combination.noMatch') }}</p>
        <div v-for="hit in matches" :key="hit.rule_id" class="rounded bg-amber-50 p-3 text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">
          <p class="font-medium">{{ hit.rule_name }} · {{ t(`admin.riskControl.combination.${hit.action}`) }}</p>
          <p>{{ hit.matches.map(word => `${word.group}: ${word.keyword}`).join(' · ') }}</p>
          <p class="mt-2 whitespace-pre-wrap break-words">{{ hit.context }}</p>
        </div>
      </div>
    </div>
    <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
    <p class="text-xs leading-5 text-gray-500">{{ t('admin.riskControl.combination.limits') }}</p>
  </div>
</template>
