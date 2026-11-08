<template>
  <span><template v-for="(part, index) in parts" :key="index"><mark v-if="index % 2 === 1" class="rounded bg-amber-200 px-0.5 font-semibold text-gray-950">{{ part }}</mark><template v-else>{{ part }}</template></template></span>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ text: string; keyword?: string; keywords?: string[] }>()
const parts = computed(() => {
  const words = [...new Set([props.keyword, ...(props.keywords ?? [])].filter((word): word is string => !!word))]
  if (!words.length) return [props.text]
  const escaped = words.sort((a, b) => b.length - a.length).map(word => word.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')
  return props.text.split(new RegExp(`(${escaped})`, 'gi'))
})
</script>
