<script setup lang="ts">
import { IconLoader2 } from '@tabler/icons-vue'

withDefaults(defineProps<{ size?: number; label?: string }>(), { size: 16 })
</script>

<template>
  <span class="inline-flex" :role="label ? 'status' : undefined" :aria-label="label" :aria-hidden="label ? undefined : 'true'">
    <IconLoader2 :size="size" stroke="1.75" class="animate-spin" />
  </span>
</template>
