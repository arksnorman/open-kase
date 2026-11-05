<script setup lang="ts">
import { ref } from 'vue'
import { useFieldControl } from './field'

const props = withDefaults(
  defineProps<{
    type?: 'text' | 'email' | 'search' | 'url' | 'password' | 'number' | 'date' | 'time'
    id?: string
    invalid?: boolean
    size?: 'sm' | 'md' | 'lg'
    mono?: boolean
  }>(),
  { type: 'text', size: 'md', mono: false },
)
// Always a string, also for type="number" (native v-model would hand back a number there).
const model = defineModel<string>({ default: '' })
const { id, describedBy, invalid, required } = useFieldControl(props)
const el = ref<HTMLInputElement | null>(null)
defineExpose({ focus: () => el.value?.focus() })
</script>

<template>
  <input
    :id="id"
    ref="el"
    :value="model"
    :type="type"
    class="lg-input"
    :class="[size === 'sm' ? 'h-[34px]' : size === 'lg' ? 'h-10 text-[14px]' : '', mono ? 'lg-mono' : '']"
    :aria-invalid="invalid ? 'true' : undefined"
    :aria-describedby="describedBy"
    :required="required || undefined"
    @input="model = ($event.target as HTMLInputElement).value"
  />
</template>
