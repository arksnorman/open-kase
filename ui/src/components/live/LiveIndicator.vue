<script setup lang="ts">
// Subtle connection state of the live-updates stream (sidebar footer).
import { computed } from 'vue'
import { useLiveState } from '@/composables/useLiveUpdates'

/** `compact`: the dot alone (collapsed sidebar rail); the label stays for screen readers. */
defineProps<{ compact?: boolean }>()
const { state, polling } = useLiveState()
const view = computed(() => {
  switch (state.value) {
    case 'live':
      return { label: 'Live', dot: 'lg-dot-resolved', title: 'Queues and the open ticket update automatically.' }
    case 'connecting':
      return { label: 'Connecting…', dot: 'lg-dot-waiting', title: 'Opening the live-updates connection.' }
    case 'reconnecting':
      return {
        label: 'Reconnecting',
        dot: 'lg-dot-waiting',
        title: polling.value
          ? 'Live updates are unavailable; refreshing every 30 seconds until the connection is back.'
          : 'Live updates were interrupted; reconnecting.',
      }
    default:
      return null
  }
})
</script>

<template>
  <div v-if="view" class="flex items-center gap-2 pt-2 pb-2 text-xs text-muted" :class="compact ? 'justify-center px-0' : 'px-4'" v-tooltip="compact ? `${view.label}. ${view.title}` : view.title">
    <span class="lg-dot" :class="view.dot" aria-hidden="true"></span>
    <span role="status" aria-live="polite" :class="compact ? 'sr-only' : ''">{{ view.label }}<span class="sr-only">. {{ view.title }}</span></span>
  </div>
</template>
