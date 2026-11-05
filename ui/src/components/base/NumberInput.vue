<script setup lang="ts">
import { ref } from 'vue'
import { useFieldControl } from './field'

const props = withDefaults(
  defineProps<{ id?: string; invalid?: boolean; size?: 'sm' | 'md'; min?: number; max?: number; step?: number }>(),
  { size: 'md' },
)
const model = defineModel<number>({ required: true })
const { id, describedBy, invalid, required } = useFieldControl(props)
const el = ref<HTMLInputElement | null>(null)
defineExpose({ focus: () => el.value?.focus() })
</script>

<template>
  <input
    :id="id"
    ref="el"
    v-model.number="model"
    type="number"
    inputmode="numeric"
    class="lg-input"
    :class="size === 'sm' ? 'h-[34px]' : ''"
    :min="min"
    :max="max"
    :step="step"
    :aria-invalid="invalid ? 'true' : undefined"
    :aria-describedby="describedBy"
    :required="required || undefined"
  />
</template>
