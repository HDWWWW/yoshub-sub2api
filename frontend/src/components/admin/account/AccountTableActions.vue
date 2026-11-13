<template>
  <div class="flex w-full min-w-0 flex-wrap items-start justify-between gap-3" data-testid="account-toolbar">
    <div v-if="!authStore.isReadOnlyAdmin" class="flex min-w-0 flex-wrap items-center gap-2" data-testid="account-toolbar-operations">
      <slot name="before"></slot>
    </div>
    <div class="ml-auto flex max-w-full flex-wrap items-center justify-end gap-2" data-testid="account-toolbar-actions">
      <button @click="$emit('refresh')" :disabled="loading" :aria-label="t('common.refresh')" :title="t('common.refresh')" class="btn btn-secondary">
        <Icon name="refresh" size="md" :class="[loading ? 'animate-spin' : '']" />
      </button>
      <slot name="after"></slot>
      <slot name="beforeCreate"></slot>
      <button v-admin-write v-if="!authStore.isReadOnlyAdmin" @click="$emit('create')" class="btn btn-primary">{{ t('admin.accounts.createAccount') }}</button>
      <slot name="afterCreate"></slot>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useAuthStore } from '@/stores/auth'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

defineProps(['loading'])
defineEmits(['refresh', 'create'])

const { t } = useI18n()
const authStore = useAuthStore()
</script>
