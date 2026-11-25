<template>
  <section v-if="wallets.length || view.muze?.token_configured" class="divide-y divide-gray-100 dark:divide-dark-700" :aria-label="t('accountRefill.supplierBalances')" data-testid="refill-supplier-balances">
    <div v-for="wallet in wallets" :key="wallet.provider" class="grid gap-3 py-3 first:pt-0 lg:grid-cols-[minmax(12rem,1fr)_minmax(0,2fr)]" :data-testid="`refill-wallet-${wallet.provider}`">
      <div class="min-w-0">
        <p class="text-sm font-semibold text-gray-900 dark:text-white">{{ wallet.provider }} <span class="ml-1 text-xs font-normal text-gray-500">{{ t('accountRefill.points') }}</span></p>
        <p class="mt-1 break-words text-xs text-gray-500">{{ t('accountRefill.snapshot') }} {{ date(wallet.updatedAt) }}</p>
      </div>
      <dl class="grid min-w-0 grid-cols-3 gap-3">
        <div v-for="item in wallet.items" :key="item.key" class="min-w-0">
          <dt class="text-xs text-gray-500">{{ t(`accountRefill.${item.key}`) }}</dt>
          <dd class="mt-1 break-all text-base font-semibold tabular-nums text-gray-900 dark:text-white">{{ number(item.value) }}</dd>
        </div>
      </dl>
    </div>
    <div v-if="view.muze?.token_configured" class="space-y-3 py-3 first:pt-0" data-testid="refill-wallet-muze">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <p class="text-sm font-semibold text-gray-900 dark:text-white">Muze <span class="ml-1 text-xs font-normal text-gray-500">{{ t('accountRefill.muzePrepaid') }}</span></p>
        <p class="text-xs text-gray-500">{{ t('accountRefill.muzeUpdated') }} {{ date(view.muze.checked_at) }}</p>
      </div>
      <div v-for="item in view.muze.products || []" :key="item.product" class="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]" data-testid="refill-muze-product">
        <div class="min-w-0 text-sm">
          <p class="break-words">{{ item.label || item.product }}</p>
          <p v-if="item.blocked_reason || item.expired || item.closed" class="mt-1 break-words text-xs text-amber-700 dark:text-amber-300">{{ item.blocked_reason || t(item.expired ? 'accountRefill.expired' : 'accountRefill.planUnavailable') }}</p>
        </div>
        <dl class="grid min-w-0 grid-cols-3 gap-3">
          <div class="min-w-0"><dt class="text-xs text-gray-500">{{ t('accountRefill.muzeRemaining') }}</dt><dd class="mt-1 break-all text-sm font-semibold tabular-nums">{{ number(item.remaining) }}</dd></div>
          <div class="min-w-0"><dt class="text-xs text-gray-500">{{ t('accountRefill.muzeInventory') }}</dt><dd class="mt-1 break-all text-sm font-semibold tabular-nums">{{ number(item.stock) }}</dd></div>
          <div class="min-w-0"><dt class="text-xs text-gray-500">{{ t('accountRefill.muzeAvailable') }}</dt><dd class="mt-1 break-all text-sm font-semibold tabular-nums">{{ number(item.available_now) }}</dd></div>
        </dl>
      </div>
      <p v-if="!view.muze.products?.length" class="text-xs text-gray-500">{{ t('accountRefill.muzeCatalogEmpty') }}</p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { RefillView } from '@/api/admin/accountRefill'

type BalanceView = Pick<RefillView, 'token_configured' | 'wallet' | 'catalog' | 'identity_at' | 'catalog_at' | 'supplier_558669' | 'muze'>
const props = defineProps<{ view: BalanceView }>()
const { t } = useI18n()
const number = (value: number | null | undefined) => value == null || !Number.isFinite(value) ? '—' : value.toLocaleString(undefined, { maximumFractionDigits: 8 })
const date = (value: string | null | undefined) => !value || value.startsWith('0001') ? '—' : new Date(value).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })

const wallets = computed(() => {
  const view = props.view
  const rows = []
  if (view.token_configured) {
    rows.push({
      provider: '1208', updatedAt: view.wallet ? view.identity_at : view.catalog_at,
      items: [
        { key: 'balance', value: view.wallet?.balance },
        { key: 'held', value: view.wallet?.held },
        { key: 'available', value: view.wallet?.available ?? view.catalog?.available }
      ]
    })
  }
  if (view.supplier_558669?.token_configured) {
    const catalog = view.supplier_558669.catalog
    const total = catalog?.available != null && catalog.reserved != null ? catalog.available + catalog.reserved : undefined
    rows.push({
      provider: '558669', updatedAt: view.supplier_558669.checked_at,
      items: [
        { key: 'balance', value: total },
        { key: 'held', value: catalog?.reserved },
        { key: 'available', value: catalog?.available }
      ]
    })
  }
  return rows
})
</script>
