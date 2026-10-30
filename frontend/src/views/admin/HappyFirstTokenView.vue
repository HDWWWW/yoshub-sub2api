<template>
  <AppLayout>
    <div class="mx-auto max-w-4xl space-y-6">
      <div class="lg:hidden">
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('happyFirstToken.title') }}</h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.description') }}</p>
      </div>
      <div v-if="loading" class="py-16 text-center" role="status" aria-busy="true">
        <div class="mx-auto h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>
      <div v-else-if="loadError" class="card p-6" role="alert">
        <p class="text-sm text-red-600 dark:text-red-400">{{ t('happyFirstToken.loadError') }}</p>
        <button type="button" class="btn btn-secondary mt-4" @click="load">{{ t('happyFirstToken.retry') }}</button>
      </div>
      <form v-else class="space-y-6" @submit.prevent="save">
        <div class="card overflow-hidden">
          <div class="flex items-center justify-between gap-6 border-b border-gray-100 px-6 py-5 dark:border-dark-700">
            <div>
              <h2 id="happy-first-token-enable" class="font-medium text-gray-900 dark:text-white">{{ t('happyFirstToken.enable') }}</h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.enableHint') }}</p>
            </div>
            <button
              type="button" role="switch" :aria-checked="enabled" aria-labelledby="happy-first-token-enable"
              class="relative inline-flex h-6 w-11 shrink-0 rounded-full transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-primary-600"
              :class="enabled ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-600'" :disabled="saving"
              @click="enabled = !enabled"
            ><span class="mt-0.5 inline-block h-5 w-5 rounded-full bg-white shadow transition-transform" :class="enabled ? 'translate-x-[22px]' : 'translate-x-0.5'"></span></button>
          </div>
          <div class="space-y-6 p-6">
            <div class="flex flex-wrap items-center justify-between gap-3">
              <div>
                <h2 class="font-medium text-gray-900 dark:text-white">{{ t('happyFirstToken.rangeTitle') }}</h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.rangeHint') }}</p>
              </div>
              <span class="rounded-full px-3 py-1 text-xs font-medium" :class="enabled ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/20 dark:text-primary-300' : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400'">{{ t(enabled ? 'happyFirstToken.enabled' : 'happyFirstToken.disabled') }}</span>
            </div>
            <div class="grid gap-5 sm:grid-cols-2">
              <div>
                <label for="happy-minimum" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('happyFirstToken.minimum') }} ({{ t('happyFirstToken.seconds') }})</label>
                <input id="happy-minimum" v-model="minimum" type="number" min="0" max="60" step="0.001" required class="input w-full tabular-nums" :disabled="saving" :aria-invalid="!valid" aria-describedby="happy-delay-limits" />
              </div>
              <div>
                <label for="happy-maximum" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('happyFirstToken.maximum') }} ({{ t('happyFirstToken.seconds') }})</label>
                <input id="happy-maximum" v-model="maximum" type="number" min="0" max="60" step="0.001" required class="input w-full tabular-nums" :disabled="saving" :aria-invalid="!valid" aria-describedby="happy-delay-limits" />
              </div>
            </div>
            <div class="rounded-lg bg-gray-50 px-4 py-4 dark:bg-dark-900/50">
              <p v-if="valid" class="font-medium tabular-nums text-gray-900 dark:text-white">{{ rangeLabel }}</p>
              <p v-else class="text-sm text-red-600 dark:text-red-400" role="alert">{{ t('happyFirstToken.invalid') }}</p>
              <p id="happy-delay-limits" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.limits') }}</p>
            </div>
            <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.timingHint') }}</p>
          </div>
          <div class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 px-6 py-4 dark:border-dark-700">
            <p class="text-sm text-gray-500 dark:text-gray-400" aria-live="polite">{{ dirty ? t('happyFirstToken.unsaved') : '' }}</p>
            <button type="submit" class="btn btn-primary" :disabled="saving || !valid || !dirty">{{ t(saving ? 'happyFirstToken.saving' : 'happyFirstToken.save') }}</button>
          </div>
        </div>
        <div class="grid gap-5 sm:grid-cols-2">
          <div v-for="item in ['scope', 'accuracy', 'privacy', 'errors']" :key="item" class="space-y-2">
            <h2 class="text-sm font-medium text-gray-900 dark:text-white">{{ t(`happyFirstToken.${item}Title`) }}</h2>
            <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t(`happyFirstToken.${item}`) }}</p>
          </div>
        </div>
      </form>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores/app'
import { getHappyFirstTokenSettings, updateHappyFirstTokenSettings, type HappyFirstTokenSettings } from '@/api/admin/settings'

const { t } = useI18n()
const app = useAppStore()
const loading = ref(true)
const loadError = ref(false)
const saving = ref(false)
const enabled = ref(false)
const minimum = ref<number | string>(0.1)
const maximum = ref<number | string>(1)
const saved = ref('')
const valid = computed(() => minimum.value !== '' && maximum.value !== '' && Number.isFinite(Number(minimum.value)) && Number.isFinite(Number(maximum.value)) && Number(minimum.value) >= 0 && Number(maximum.value) <= 60 && Number(minimum.value) <= Number(maximum.value))
const payload = computed<HappyFirstTokenSettings>(() => ({ enabled: enabled.value, min_delay_ms: Math.round(Number(minimum.value) * 1000), max_delay_ms: Math.round(Number(maximum.value) * 1000) }))
const dirty = computed(() => JSON.stringify(payload.value) !== saved.value)
const rangeLabel = computed(() => Number(minimum.value) === Number(maximum.value)
  ? t('happyFirstToken.fixed', { seconds: minimum.value })
  : t('happyFirstToken.random', { min: minimum.value, max: maximum.value }))

function apply(settings: HappyFirstTokenSettings) {
  enabled.value = settings.enabled
  minimum.value = settings.min_delay_ms / 1000
  maximum.value = settings.max_delay_ms / 1000
  saved.value = JSON.stringify(payload.value)
}

async function load() {
  loading.value = true
  loadError.value = false
  try { apply(await getHappyFirstTokenSettings()) }
  catch { loadError.value = true }
  finally { loading.value = false }
}

async function save() {
  if (!valid.value || saving.value) return
  saving.value = true
  try {
    apply(await updateHappyFirstTokenSettings(payload.value))
    app.showSuccess(t('happyFirstToken.saved'))
  } catch { app.showError(t('happyFirstToken.saveError')) }
  finally { saving.value = false }
}

onMounted(load)
</script>
