<script setup lang="ts">
// Filter chip (Ledger "Status: Any ▾") built on SelectInput: a listbox with checkboxes
// (multiple) or a single choice, with search when the list is long, type-ahead, and an
// optional "Add …" row (`create`) for free values such as tags. The chip text always names the
// current selection; values without a known option (e.g. a free tag) still show.
import { IconSelector } from '@tabler/icons-vue'
import { computed } from 'vue'
import SelectInput from './SelectInput.vue'
import type { SelectOption } from './types'

const props = withDefaults(
  defineProps<{
    label: string
    options: SelectOption<string>[]
    multiple?: boolean
    /** Text for an empty selection, e.g. "Any". */
    anyLabel?: string
    disabled?: boolean
    searchable?: boolean
    create?: (query: string) => SelectOption<string> | null
    /** Async options searched on the server (replaces the list while open); `options` then only names known values. */
    loader?: (query: string, signal: AbortSignal) => Promise<SelectOption<string>[]>
  }>(),
  { multiple: true, anyLabel: 'Any', disabled: false, searchable: undefined, create: undefined, loader: undefined },
)
const model = defineModel<string[]>({ required: true })
defineSlots<{
  option?(p: { option: SelectOption<string>; selected: boolean; active: boolean }): unknown
  default?(): unknown
}>()

const inner = computed<string[] | string | '' | null>({
  get: () => (props.multiple ? model.value : (model.value[0] ?? '')),
  set: (v) => {
    model.value = Array.isArray(v) ? v : v ? [v] : []
  },
})

const summary = computed(() => {
  const chosen = model.value.map((v) => props.options.find((o) => o.value === v)?.label ?? v)
  if (!chosen.length) return props.anyLabel
  if (chosen.length <= 2) return chosen.join(', ')
  return `${chosen[0]} +${chosen.length - 1}`
})
const active = computed(() => model.value.length > 0)
</script>

<template>
  <SelectInput
    v-model="inner"
    :options="options"
    :multiple="multiple"
    :label="label"
    :disabled="disabled"
    :searchable="searchable"
    :create="create"
    :loader="loader"
    :search-placeholder="`Find ${label.toLowerCase()}`"
    empty-text="Nothing to choose from"
    panel-class="w-64"
  >
    <template #trigger="{ attrs, open }">
      <button
        type="button"
        v-bind="attrs"
        class="lg-chip lg-hov"
        :class="[active ? '!border-border-strong' : '', open ? '!border-text' : '']"
        :disabled="disabled"
        :aria-label="`${label}: ${summary}`"
      >
        {{ label }}: <strong class="lg-ell max-w-40 font-semibold">{{ summary }}</strong>
        <IconSelector :size="14" stroke="1.75" aria-hidden="true" />
      </button>
    </template>
    <template v-if="$slots.option" #option="p"><slot name="option" :option="p.option as SelectOption<string>" :selected="p.selected" :active="p.active" /></template>
    <template v-if="active || $slots.default" #footer="{ close }">
      <button
        v-if="active"
        type="button"
        class="lg-nav lg-hov w-full border-0 bg-transparent text-left text-muted"
        @mousedown.prevent
        @click="
          model = [];
          close()
        "
      >
        Clear {{ label.toLowerCase() }}
      </button>
      <slot />
    </template>
  </SelectInput>
</template>
