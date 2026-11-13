<template>
  <ReadOnlySettingsView v-if="authStore.isReadOnlyAdmin" />
  <SettingsView v-else />
</template>

<script setup lang="ts">
import { defineAsyncComponent } from 'vue'
import { useAuthStore } from '@/stores/auth'
import ReadOnlySettingsView from './ReadOnlySettingsView.vue'
const SettingsView = defineAsyncComponent(() => import('./SettingsView.vue'))
const authStore = useAuthStore()
</script>
