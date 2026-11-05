<script setup lang="ts">
// Colour-coded priority, shown by name ("High"). `short` drops the border/extra spacing for the compact
// ticket header; the full label stays in
// the tooltip and the accessible name.
import type { Priority } from '@/api/types'
import { priorityLong, priorityShort } from '@/format'

defineProps<{ priority: Priority; short?: boolean }>()
</script>

<template>
  <span v-if="short" class="lg-pill" :class="`lg-prio-${priority}`" role="img" v-tooltip="priorityLong(priority)" :aria-label="priorityLong(priority)">{{ priorityShort(priority) }}</span>
  <span v-else class="lg-pill" :class="`lg-prio-${priority}`" v-tooltip="priorityLong(priority)">{{ priorityShort(priority) }}</span>
</template>
