<template>
  <div class="space-y-1 whitespace-nowrap text-xs">
    <div><span class="text-gray-500">{{ t('accountRefill.supplierReclaimAt') }}：</span><span>{{ metadata?.source_expires_at ? formatSupplierTime(metadata.source_expires_at) : metadata?.source_expiry_origin === 'supplier_pending' ? t('accountRefill.supplierExpiryPending') : '—' }}</span></div>
    <div><span class="text-gray-500">{{ t('accountRefill.effectiveDeadline') }}：</span><span>{{ metadata ? (metadata.effective_expires_at ? formatSupplierTime(metadata.effective_expires_at) : t('accountRefill.noDeadline')) : t(failed ? 'accountRefill.metadataFailed' : 'common.loading') }}</span></div>
    <div v-if="metadata" class="flex gap-2 text-gray-500">
      <span v-if="metadata.grace_minutes">{{ t('accountRefill.retentionLabel', { minutes: metadata.grace_minutes }) }}</span>
      <span v-if="!autoPause">{{ t('accountRefill.autoPauseDisabled') }}</span>
      <span v-else-if="expired" class="text-amber-600">{{ t('admin.accounts.expired') }}</span>
      <span v-else>{{ t('admin.accounts.autoPauseOnExpired') }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatSupplierTime } from '@/utils/supplierTime'
import type { AccountSchedulingMetadata } from '@/api/admin/accountScheduling'
const props = defineProps<{ metadata?: AccountSchedulingMetadata; autoPause: boolean; failed: boolean }>()
const { t } = useI18n()
const expired = computed(() => props.metadata?.effective_expires_at && Date.parse(props.metadata.effective_expires_at) <= Date.now())
</script>
