<script setup lang="ts">
// Status picker: SelectInput with StatusBadge (colour + dot shape per lifecycle category) in
// options and value. `currentId` marks the ticket's present status as "current" and disabled.
// Extra attributes (size, disabled, aria-label, placeholder, id) pass through to SelectInput.
import { computed, ref } from 'vue'
import type { TicketStatus } from '@/api/types'
import { categoryLabel } from '@/format'
import SelectInput from './SelectInput.vue'
import StatusBadge from './StatusBadge.vue'
import type { SelectOption } from './types'

const props = defineProps<{
  statuses: TicketStatus[]
  currentId?: string
  /** Optional note per status, shown beside its badge (e.g. "runs Resolve"). */
  describe?: (s: TicketStatus) => string | undefined
  /** Adds a first option with value "" and this label (e.g. "Keep current status"). */
  noneLabel?: string
}>()
const model = defineModel<string | null>({ default: '' })
const byId = computed(() => new Map(props.statuses.map((s) => [s.id, s])))
const options = computed<SelectOption<string>[]>(() => [
  ...(props.noneLabel ? [{ value: '', label: props.noneLabel }] : []),
  ...props.statuses.map((s) => ({
    value: s.id,
    label: s.name,
    keywords: categoryLabel(s.category),
    disabled: s.id === props.currentId,
    hint: s.id === props.currentId ? 'current' : props.describe?.(s),
  })),
])
const inner = ref<{ open: () => void; focus: () => void } | null>(null)
defineExpose({ open: () => inner.value?.open(), focus: () => inner.value?.focus() })
</script>

<template>
  <SelectInput ref="inner" v-model="model" :options="options">
    <template #value="{ option }">
      <StatusBadge v-if="byId.get(option.value)" :status="byId.get(option.value)!" />
      <span v-else class="lg-ell">{{ option.label }}</span>
    </template>
    <template #option="{ option }">
      <span class="flex-1">
        <StatusBadge v-if="byId.get(option.value)" :status="byId.get(option.value)!" />
        <span v-else>{{ option.label }}</span>
      </span>
      <span v-if="option.hint" class="text-xs font-normal text-muted">{{ option.hint }}</span>
    </template>
  </SelectInput>
</template>
