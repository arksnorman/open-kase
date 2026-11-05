<script setup lang="ts">
// Inline loading line: spinner plus text, announced politely. One look for every pending fetch.
import Spinner from './Spinner.vue'

withDefaults(defineProps<{ text?: string; size?: 'sm' | 'md' }>(), { text: 'Loading…', size: 'md' })
</script>

<template>
  <div role="status" class="flex items-center gap-2 text-muted">
    <Spinner :size="size === 'sm' ? 12 : 14" />
    <span><slot>{{ text }}</slot></span>
  </div>
</template>
