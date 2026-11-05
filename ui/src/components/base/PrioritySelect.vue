<script setup lang="ts">
// Priority picker: SelectInput with the colour-coded PriorityBadge in options and value.
// Extra attributes (size, disabled, aria-label, placeholder, id) pass through to SelectInput.
import { ref } from 'vue'
import type { Priority } from '@/api/types'
import { PRIORITIES, priorityLong } from '@/format'
import PriorityBadge from './PriorityBadge.vue'
import SelectInput from './SelectInput.vue'
import type { SelectOption } from './types'

const model = defineModel<Priority | '' | null>({ default: '' })
const options: SelectOption<Priority>[] = PRIORITIES.map((p) => ({ value: p, label: priorityLong(p), keywords: `P${p}` }))
const inner = ref<{ open: () => void; focus: () => void } | null>(null)
defineExpose({ open: () => inner.value?.open(), focus: () => inner.value?.focus() })
</script>

<template>
  <SelectInput ref="inner" v-model="model" :options="options" :searchable="false">
    <template #value="{ option }"><PriorityBadge :priority="option.value" /></template>
    <template #option="{ option }">
      <PriorityBadge :priority="option.value" />
      <span class="lg-ell flex-1 text-xs font-normal text-muted">Priority {{ option.value }}</span>
    </template>
  </SelectInput>
</template>
