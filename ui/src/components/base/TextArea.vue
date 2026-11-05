<script setup lang="ts">
import { ref } from 'vue'
import { useFieldControl } from './field'

const props = withDefaults(defineProps<{ id?: string; invalid?: boolean; rows?: number; tone?: 'default' | 'note' }>(), {
  rows: 4,
  tone: 'default',
})
const model = defineModel<string>({ default: '' })
const { id, describedBy, invalid, required } = useFieldControl(props)
const el = ref<HTMLTextAreaElement | null>(null)
defineExpose({ focus: () => el.value?.focus() })
</script>

<template>
  <textarea
    :id="id"
    ref="el"
    v-model="model"
    :rows="rows"
    class="lg-input"
    :class="tone === 'note' ? '!bg-note-bg' : ''"
    :aria-invalid="invalid ? 'true' : undefined"
    :aria-describedby="describedBy"
    :required="required || undefined"
  ></textarea>
</template>
