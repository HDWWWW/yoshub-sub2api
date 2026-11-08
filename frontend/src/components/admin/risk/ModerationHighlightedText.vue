<template>
  <span><template v-for="(part, index) in parts" :key="index"><mark v-if="index % 2 === 1" class="rounded bg-amber-200 px-0.5 font-semibold text-gray-950">{{ part }}</mark><template v-else>{{ part }}</template></template></span>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ text: string; keyword?: string }>()
const parts = computed(() => {
  if (!props.keyword) return [props.text]
  const escaped = props.keyword.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return props.text.split(new RegExp(`(${escaped})`, 'gi'))
})
</script>
