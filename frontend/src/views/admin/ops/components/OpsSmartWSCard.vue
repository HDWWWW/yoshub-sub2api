<template>
  <section class="rounded-xl border border-gray-200 bg-white p-5 dark:border-dark-600 dark:bg-dark-800" aria-labelledby="smart-ws-title">
    <h3 id="smart-ws-title" class="font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.ops.smartWS.title') }}</h3>
    <p class="mt-1 text-xs text-gray-500">{{ t('admin.ops.smartWS.scope') }}</p>
    <p v-if="error" class="mt-3 text-sm text-red-600" role="alert">{{ t('admin.ops.smartWS.loadFailed') }}</p>
    <p v-else-if="!data" class="mt-3 text-sm text-gray-500">{{ t('admin.ops.loadingText') }}</p>
    <template v-if="data">
      <dl class="mt-4 grid grid-cols-2 gap-4 md:grid-cols-4 xl:grid-cols-6">
        <div v-for="item in items" :key="item.key">
          <dt class="text-xs text-gray-500">{{ t(`admin.ops.smartWS.${item.key}`) }}</dt>
          <dd class="mt-1 font-mono text-lg text-gray-900 dark:text-gray-100">{{ item.value }}</dd>
        </div>
      </dl>
      <div class="mt-5 overflow-x-auto">
        <table class="w-full text-left text-sm">
          <caption class="mb-2 text-left text-xs text-gray-500">{{ t('admin.ops.smartWS.latencyHint') }}</caption>
          <thead class="text-xs text-gray-500"><tr><th>{{ t('admin.ops.smartWS.connection') }}</th><th>{{ t('admin.ops.smartWS.samples') }}</th><th>TTFT P50</th><th>TTFT P95</th><th>{{ t('admin.ops.smartWS.tail') }}</th></tr></thead>
          <tbody class="text-gray-800 dark:text-gray-200">
            <tr v-for="kind in ['cold', 'reused'] as const" :key="kind" class="border-t border-gray-100 dark:border-dark-600">
              <td class="py-2">{{ t(`admin.ops.smartWS.${kind}`) }}</td>
              <td>{{ data[kind].ttft_samples }} / {{ data[kind].samples }}</td>
              <td>{{ latency(data[kind].ttft_p50_ms, data[kind].ttft_samples) }}</td>
              <td>{{ latency(data[kind].ttft_p95_ms, data[kind].ttft_samples) }}</td>
              <td>{{ latency(data[kind].duration_p95_ms, data[kind].samples) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getSmartWSMetrics, type SmartWSMetrics } from '@/api/admin/ops'

const props = defineProps<{ refreshToken: number }>()
const { t } = useI18n()
const data = ref<SmartWSMetrics | null>(null)
const error = ref(false)
let controller: AbortController | undefined
const latency = (ms: number, count: number) => count ? `${ms.toLocaleString()} ms` : '—'
const items = computed(() => {
  if (!data.value) return []
  const d = data.value
  const c = d.counters
  return [
    { key: 'connections', value: `${d.connections} / ${d.limits.connections}` },
    { key: 'active', value: d.active },
    { key: 'acquiring', value: `${d.acquiring} / ${d.limits.acquiring}` },
    { key: 'handshaking', value: `${d.handshaking} / ${d.limits.handshakes}` },
    ...['attempted', 'succeeded', 'background_accepted', 'failed', 'incomplete', 'canceled', 'handshakes', 'handshake_failed', 'reused', 'safe_http_fallback', 'queue_rejected', 'capacity_rejected', 'upstream_terminal_error', 'transport_error', 'dispatch_uncertain', 'handshake_rate_limited', 'handshake_401', 'handshake_403', 'handshake_rejection', 'rejection_bypass', 'continuation_unavailable', 'handshake_unsupported', 'queued', 'queue_wait_ms'].map(key => ({ key, value: c[key] ?? 0 }))
  ]
})
watch(() => props.refreshToken, async () => {
  controller?.abort()
  const current = new AbortController()
  controller = current
  try {
    const next = await getSmartWSMetrics({ signal: current.signal })
    if (!current.signal.aborted) { data.value = next; error.value = false }
  } catch {
    if (!current.signal.aborted) error.value = true
  }
}, { immediate: true })
onBeforeUnmount(() => controller?.abort())
</script>
