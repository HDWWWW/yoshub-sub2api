<template>
  <div class="mt-3 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
    <button type="button" class="text-sm font-medium text-primary-600 dark:text-primary-400" :aria-expanded="expanded" data-testid="expiry-grace-open" @click="open">
      {{ t('accountRefill.expiryGraceTitle') }}
    </button>
    <div v-if="expanded" class="mt-3 space-y-3">
      <p v-if="loading" class="text-sm text-gray-500" role="status">{{ t('common.loading') }}</p>
      <template v-else-if="loaded">
        <label class="block">
          <span class="input-label">{{ t('accountRefill.expiryGraceMinutes') }}</span>
          <input v-model.number="minutes" type="number" min="0" max="1440" step="1" class="input" :disabled="saving" :aria-invalid="!valid" data-testid="expiry-grace-minutes" />
        </label>
        <div class="flex flex-wrap gap-2">
          <button v-for="preset in [0, 30, 60, 120]" :key="preset" type="button" class="btn btn-secondary text-xs" :disabled="saving" @click="minutes = preset">{{ t('accountRefill.minutePreset', { minutes: preset }) }}</button>
        </div>
        <p class="text-xs leading-5 text-gray-500">{{ t('accountRefill.expiryGraceHint') }}</p>
        <p class="text-xs text-gray-500">{{ t('accountRefill.supplierReclaimAt') }}: {{ sourceExpiry ? new Date(sourceExpiry).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }) : '—' }}</p>
        <p v-if="effectiveExpiry" class="text-xs text-gray-500">{{ t('accountRefill.effectiveExpiry') }}: {{ new Date(effectiveExpiry).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }) }}</p>
        <button type="button" class="btn btn-secondary" :disabled="saving || !valid || minutes === saved" data-testid="expiry-grace-save" @click="save">{{ t(saving ? 'accountRefill.saving' : 'accountRefill.saveExpiryGrace') }}</button>
        <p v-if="success" class="text-xs text-green-600" role="status">{{ t('accountRefill.expiryGraceSaved') }}</p>
      </template>
      <p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { accountExpiryGraceAPI } from '@/api/admin/accountExpiryGrace'

const props = defineProps<{ accountId: number; active: boolean }>()
const { t } = useI18n()
const expanded = ref(false), loading = ref(false), loaded = ref(false), saving = ref(false), success = ref(false)
const minutes = ref(0), saved = ref(0), effectiveExpiry = ref<string | null>(null), sourceExpiry = ref<string | null>(null), error = ref('')
const valid = computed(() => Number.isInteger(minutes.value) && minutes.value >= 0 && minutes.value <= 1440)
let generation = 0
watch(() => [props.accountId, props.active], () => {
  generation++; expanded.value = false; loaded.value = false; loading.value = false; saving.value = false; error.value = ''; success.value = false
})
async function open() {
  if (loading.value || saving.value) return
  expanded.value = !expanded.value
  if (!expanded.value || loaded.value || !props.active) return
  const current = generation
  loading.value = true; error.value = ''
  try {
    const result = await accountExpiryGraceAPI.get(props.accountId)
    if (current !== generation) return
    minutes.value = saved.value = result.minutes; effectiveExpiry.value = result.effective_expires_at; sourceExpiry.value = result.source_expires_at; loaded.value = true
  } catch { if (current === generation) error.value = t('accountRefill.expiryGraceError') }
  finally { if (current === generation) loading.value = false }
}
async function save() {
  if (!valid.value || saving.value || !props.active) return
  const current = generation
  saving.value = true; error.value = ''; success.value = false
  try {
    const result = await accountExpiryGraceAPI.save(props.accountId, minutes.value)
    if (current !== generation) return
    minutes.value = saved.value = result.minutes; effectiveExpiry.value = result.effective_expires_at; sourceExpiry.value = result.source_expires_at; success.value = true
  } catch { if (current === generation) error.value = t('accountRefill.expiryGraceError') }
  finally { if (current === generation) saving.value = false }
}
</script>
