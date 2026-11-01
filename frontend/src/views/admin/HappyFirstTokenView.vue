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
            <div class="flex items-center justify-between gap-6">
              <div>
                <h2 id="happy-delta-label" class="font-medium text-gray-900 dark:text-white">{{ t('happyFirstToken.deltaMode') }}</h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('happyFirstToken.deltaHint') }}</p>
              </div>
              <button id="happy-delta" type="button" role="switch" :aria-checked="deltaMode" aria-labelledby="happy-delta-label"
                class="relative inline-flex h-6 w-11 shrink-0 rounded-full transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-primary-600"
                :class="deltaMode ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-600'" :disabled="saving" @click="deltaMode = !deltaMode"
              ><span class="mt-0.5 inline-block h-5 w-5 rounded-full bg-white shadow transition-transform" :class="deltaMode ? 'translate-x-[22px]' : 'translate-x-0.5'"></span></button>
            </div>
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
            <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
              <h2 class="font-medium text-gray-900 dark:text-white">{{ t('happyFirstToken.groupTitle') }}</h2>
              <p class="text-sm leading-6 text-gray-500">{{ t('happyFirstToken.groupHint') }}</p>
              <input v-model="groupSearch" class="input" :placeholder="t('happyFirstToken.searchGroups')" :aria-label="t('happyFirstToken.searchGroups')" />
              <div v-for="group in visibleGroups" :key="group.id" class="rounded-lg border border-gray-200 p-4 dark:border-dark-700">
                <label class="flex items-center gap-3 text-sm font-medium"><input type="checkbox" :checked="!!overrideFor(group.id)" :disabled="saving" :data-testid="`happy-group-${group.id}`" @change="toggleGroup(group.id)" />{{ group.name }} <span class="font-normal text-gray-500">#{{ group.id }} · {{ t(overrideFor(group.id) ? 'happyFirstToken.customGroup' : 'happyFirstToken.inherit') }}</span></label>
                <template v-for="override in overrides.filter(g => g.group_id === group.id)" :key="override.group_id">
                  <label class="mt-4 block text-sm"><span class="input-label">{{ t('happyFirstToken.deltaMode') }}</span>
                    <select v-model="override.deltaMode" class="input" :disabled="saving" :data-testid="`happy-group-delta-${group.id}`">
                      <option value="inherit">{{ t('happyFirstToken.inherit') }}</option>
                      <option value="enabled">{{ t('happyFirstToken.enabled') }}</option>
                      <option value="disabled">{{ t('happyFirstToken.disabled') }}</option>
                    </select>
                  </label>
                  <div class="mt-4 grid items-end gap-4 sm:grid-cols-3">
                    <label class="flex items-center gap-2 pb-3 text-sm"><input v-model="override.enabled" type="checkbox" :disabled="saving" />{{ t('happyFirstToken.groupEnable') }}</label>
                    <label><span class="input-label">{{ t('happyFirstToken.minimum') }} ({{ t('happyFirstToken.seconds') }})</span><input v-model="override.minimum" class="input" type="number" min="0" max="60" step="0.001" required :disabled="saving" :aria-invalid="!validRange(override.minimum, override.maximum)" /></label>
                    <label><span class="input-label">{{ t('happyFirstToken.maximum') }} ({{ t('happyFirstToken.seconds') }})</span><input v-model="override.maximum" class="input" type="number" min="0" max="60" step="0.001" required :disabled="saving" :aria-invalid="!validRange(override.minimum, override.maximum)" /></label>
                  </div>
                </template>
              </div>
              <p v-if="!visibleGroups.length" class="text-sm text-gray-500">{{ t('happyFirstToken.noGroups') }}</p>
              <div v-for="override in overrides.filter(o => !groups.some(g => g.id === o.group_id))" :key="override.group_id" class="flex justify-between text-sm text-amber-700"><span>{{ t('happyFirstToken.missingGroup') }} #{{ override.group_id }}</span><button type="button" :disabled="saving" @click="toggleGroup(override.group_id)">{{ t('happyFirstToken.removeOverride') }}</button></div>
            </section>
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
import { getAll } from '@/api/admin/groups'
import type { AdminGroup } from '@/types'
import { getHappyFirstTokenSettings, updateHappyFirstTokenSettings, type HappyFirstTokenSettings } from '@/api/admin/settings'

const { t } = useI18n()
const app = useAppStore()
const loading = ref(true)
const loadError = ref(false)
const saving = ref(false)
const enabled = ref(false)
const deltaMode = ref(false)
const minimum = ref<number | string>(0.1)
const maximum = ref<number | string>(1)
const saved = ref('')
const groups = ref<AdminGroup[]>([])
const overrides = ref<{ group_id: number; enabled: boolean; deltaMode: string; minimum: number | string; maximum: number | string }[]>([])
const groupSearch = ref('')
const visibleGroups = computed(() => groups.value.filter(g => g.name.toLowerCase().includes(groupSearch.value.toLowerCase())))
const overrideFor = (id: number) => overrides.value.find(g => g.group_id === id)
function toggleGroup(id: number) {
  if (overrideFor(id)) overrides.value = overrides.value.filter(g => g.group_id !== id)
  else overrides.value.push({ group_id: id, enabled: enabled.value, deltaMode: 'inherit', minimum: minimum.value, maximum: maximum.value })
}
const validRange = (min: number | string, max: number | string) => min !== '' && max !== '' && Number.isFinite(Number(min)) && Number.isFinite(Number(max)) && Number(min) >= 0 && Number(max) <= 60 && Number(min) <= Number(max)
const valid = computed(() => validRange(minimum.value, maximum.value) && overrides.value.every(g => validRange(g.minimum, g.maximum)))
const payload = computed<HappyFirstTokenSettings>(() => ({ enabled: enabled.value, delta_mode: deltaMode.value, min_delay_ms: Math.round(Number(minimum.value) * 1000), max_delay_ms: Math.round(Number(maximum.value) * 1000), group_overrides: overrides.value.map(g => ({ group_id: g.group_id, enabled: g.enabled, ...(g.deltaMode === 'inherit' ? {} : { delta_mode: g.deltaMode === 'enabled' }), min_delay_ms: Math.round(Number(g.minimum) * 1000), max_delay_ms: Math.round(Number(g.maximum) * 1000) })) }))
const dirty = computed(() => JSON.stringify(payload.value) !== saved.value)
const rangeLabel = computed(() => Number(minimum.value) === Number(maximum.value)
  ? t('happyFirstToken.fixed', { seconds: minimum.value })
  : t('happyFirstToken.random', { min: minimum.value, max: maximum.value }))

function apply(settings: HappyFirstTokenSettings) {
  enabled.value = settings.enabled
  deltaMode.value = settings.delta_mode ?? false
  minimum.value = settings.min_delay_ms / 1000
  maximum.value = settings.max_delay_ms / 1000
  overrides.value = (settings.group_overrides || []).map(g => ({ group_id: g.group_id, enabled: g.enabled, deltaMode: g.delta_mode == null ? 'inherit' : g.delta_mode ? 'enabled' : 'disabled', minimum: g.min_delay_ms / 1000, maximum: g.max_delay_ms / 1000 }))
  saved.value = JSON.stringify(payload.value)
}

async function load() {
  loading.value = true
  loadError.value = false
  try { const [settings, all] = await Promise.all([getHappyFirstTokenSettings(), getAll('openai')]); groups.value = all; apply(settings) }
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
