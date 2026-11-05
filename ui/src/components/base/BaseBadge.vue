<script setup lang="ts">
// Small label. Tones: neutral, note (agent-only, amber), public (requester-visible, green),
// success (a positive state: active, on, sent, configured; green),
// strong (emphasised neutral), warn/danger (always paired with text),
// key (monospace identifier such as a project key). `removable` adds an × button (tag lists);
// `to` renders a RouterLink.
import { IconX } from '@tabler/icons-vue'
import { RouterLink, type RouteLocationRaw } from 'vue-router'

withDefaults(
  defineProps<{ tone?: 'neutral' | 'note' | 'public' | 'requester' | 'success' | 'strong' | 'warn' | 'danger' | 'key'; removable?: boolean; removeLabel?: string; removeDisabled?: boolean; to?: RouteLocationRaw }>(),
  { tone: 'neutral', removable: false, removeDisabled: false },
)
const emit = defineEmits<{ remove: [] }>()
</script>

<template>
  <component
    :is="to ? RouterLink : 'span'"
    :to="to"
    :class="[
      tone === 'key' ? 'lg-mono inline-flex rounded border border-border-strong px-[5px] py-0.5 text-[11px] font-medium' : 'lg-tag',
      tone === 'note' ? 'lg-tag-note' : tone === 'public' ? 'lg-tag-public' : tone === 'success' ? 'lg-tag-success' : tone === 'requester' ? 'lg-tag-requester' : tone === 'strong' ? 'lg-tag-strong' : tone === 'warn' ? '!border-warn !bg-warn-bg !text-warn' : tone === 'danger' ? '!border-danger !bg-danger-bg !text-danger' : '',
      removable ? '!pr-0.5 !text-text' : '',
      to ? 'lg-hov !text-text no-underline' : '',
    ]"
  >
    <slot name="icon" />
    <slot />
    <button
      v-if="removable"
      type="button"
      class="lg-hov inline-flex size-4 items-center justify-center rounded-sm border-0 bg-transparent p-0 text-muted"
      :aria-label="removeLabel ?? 'Remove'"
      v-tooltip="removeLabel ?? 'Remove'"
      :disabled="removeDisabled"
      @click="emit('remove')"
    >
      <IconX :size="12" stroke="2" aria-hidden="true" />
    </button>
  </component>
</template>
