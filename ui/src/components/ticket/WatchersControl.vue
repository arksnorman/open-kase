<script setup lang="ts">
// "Watchers · N" / "Watching · N" control: one button that opens a non-modal popover with a
// "Watch this ticket" switch (you) and the list of watchers who can currently read the ticket.
// The API only lets people watch or unwatch themselves, so there is no add/remove-others control.
// Esc, outside click and focus leaving close it; BasePopover returns focus to the button.
// The list is loaded on first open (GET .../watchers, staff only, access-filtered).
import { IconEye } from '@tabler/icons-vue'
import { computed, ref, useId, watch } from 'vue'
import { ticketsApi } from '@/api/endpoints'
import type { Ticket } from '@/api/types'
import { BaseAlert, BaseAvatar, BasePopover, LoadingState, SwitchInput } from '@/components/base'
import { useQuery } from '@/composables/useQuery'
import { useSessionStore } from '@/stores/session'

const props = defineProps<{ ticket: Ticket; queryBase: string; busy: boolean; archived: boolean; error?: unknown }>()
const emit = defineEmits<{ toggle: [] }>()
const session = useSessionStore()
const uid = useId()

const open = ref(false)
const everOpened = ref(false)
function onOpenChange(v: boolean) {
  open.value = v
  if (v) everOpened.value = true
}

const q = useQuery(
  () => (everOpened.value && props.queryBase ? `${props.queryBase}:watchers` : null),
  (signal) => ticketsApi.watchers(props.ticket.id, { signal }),
)
const watchers = computed(() => q.data.value?.items ?? [])
const hidden = computed(() => Math.max(0, props.ticket.watcherCount - watchers.value.length))
// Refetch (keeping the shown list) when the watch state changes while open.
watch(
  () => [props.ticket.watcherCount, props.ticket.watching],
  () => {
    if (everOpened.value) void q.refetch()
  },
)
// Archived tickets cannot be newly watched, but an existing watch can still be removed.
// Never `disabled` while saving: that would drop focus from the switch; repeat clicks are ignored.
const switchDisabled = computed(() => props.archived && !props.ticket.watching)
function onSwitch() {
  if (!props.busy) emit('toggle')
}
const label = computed(() => `${props.ticket.watching ? 'Watching' : 'Watchers'} · ${props.ticket.watcherCount}`)
</script>

<template>
  <BasePopover :open="open" placement="bottom-end" panel-class="w-72 p-3" @update:open="onOpenChange">
    <template #trigger="{ panelId, toggle }">
      <button
        type="button"
        class="lg-btn lg-hov"
        :class="ticket.watching ? 'border-accent! bg-accent-soft! text-accent-text!' : ''"
        :aria-expanded="open"
        :aria-controls="open ? panelId : undefined"
        aria-haspopup="dialog"
        :aria-label="`${label}. ${ticket.watching ? 'You are watching' : 'You are not watching'}. Manage watchers`"
        @click="toggle"
      >
        <IconEye :size="16" stroke="1.75" aria-hidden="true" />
        {{ label }}
      </button>
    </template>
    <div role="dialog" aria-label="Watchers" class="flex flex-col gap-2">
      <div class="flex items-center justify-between gap-3">
        <span :id="`${uid}-label`" class="font-medium">Watch this ticket</span>
        <SwitchInput
          :model-value="ticket.watching"
          :labelledby="`${uid}-label`"
          :describedby="`${uid}-hint`"
          :aria-disabled="busy ? 'true' : undefined"
          :disabled="switchDisabled"
          @update:model-value="onSwitch"
        />
      </div>
      <p :id="`${uid}-hint`" class="m-0 text-xs text-muted">Watchers get updates by email. Watching grants no access.</p>
      <BaseAlert v-if="error" :error="error" />
      <LoadingState v-if="q.loading.value && !q.data.value" text="Loading watchers…" size="sm" />
      <BaseAlert v-else-if="q.error.value && !q.data.value" :error="q.error.value" />
      <p v-else-if="!watchers.length && !hidden" class="m-0 text-muted">Nobody is watching this ticket.</p>
      <ul v-else-if="watchers.length" class="m-0 flex list-none flex-wrap gap-1.5 p-0" aria-label="People watching">
        <li
          v-for="w in watchers"
          :key="w.id"
          class="inline-flex max-w-full items-center gap-1.5 rounded-full border border-border py-0.5 pr-2.5 pl-0.5"
          v-tooltip="w.username ? `${w.name} (@${w.username})` : w.name"
        >
          <BaseAvatar :name="w.name" size="xs" role="staff" decorative />
          <span class="lg-ell text-xs">{{ w.name }}<template v-if="w.id === session.user?.id"> (you)</template></span>
        </li>
      </ul>
      <p v-if="q.data.value && hidden" class="m-0 text-xs text-muted">
        {{ hidden }} {{ hidden === 1 ? 'watcher has' : 'watchers have' }} no access to this ticket now and {{ hidden === 1 ? 'is' : 'are' }} not listed.
      </p>
    </div>
  </BasePopover>
</template>
