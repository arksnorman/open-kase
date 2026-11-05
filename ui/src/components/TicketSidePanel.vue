<script setup lang="ts">
// Right panel of the ticket workspace: Details · Activity. The Details content is
// supplied by the ticket view (slot "details"), which owns the ticket mutations.
import { IconX } from '@tabler/icons-vue'
import { ref } from 'vue'
import type { Ticket } from '@/api/types'
import { BaseTabs, IconButton, type TabDef } from '@/components/base'
import ActivityTimeline from './ticket/ActivityTimeline.vue'

defineProps<{
  ticket: Ticket
  /** Cache key prefix of the open ticket (activity is cached under it). */
  queryBase: string
  /** Shown over the thread in a drawer (narrow workspace): full width, with a close button. */
  overlay?: boolean
}>()
const emit = defineEmits<{
  close: []
}>()

type TabId = 'details' | 'activity'
const tabs: TabDef<TabId>[] = [
  { id: 'details', label: 'Details' },
  { id: 'activity', label: 'Activity' },
]
const selected = ref<TabId>('details')
defineExpose({ select: (id: TabId) => (selected.value = id) })
</script>

<template>
  <aside
    aria-label="Ticket side panel"
    class="flex flex-col bg-surface"
    :class="overlay ? 'min-h-0 w-full flex-1' : 'w-[360px] flex-none border-l border-border'"
  >
    <BaseTabs v-model="selected" :tabs="tabs" label="Side panel">
      <template v-if="overlay" #extra>
        <span class="flex h-full items-center">
          <IconButton label="Close panel" @click="emit('close')"><IconX :size="16" stroke="1.75" /></IconButton>
        </span>
      </template>
      <template #details>
        <h2 class="sr-only">Details</h2>
        <slot name="details" />
      </template>
      <template #activity>
        <div class="flex min-h-0 flex-1 flex-col gap-4 overflow-auto p-4">
          <h2 class="sr-only">Activity</h2>
          <ActivityTimeline v-if="selected === 'activity'" :ticket="ticket" :query-base="queryBase" />
        </div>
      </template>
    </BaseTabs>
  </aside>
</template>
