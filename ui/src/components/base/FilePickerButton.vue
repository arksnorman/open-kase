<script setup lang="ts">
// Button that opens the system file picker and emits the chosen files.
import { IconPaperclip } from '@tabler/icons-vue'
import { ref } from 'vue'
import BaseButton from './BaseButton.vue'

withDefaults(defineProps<{ label?: string; multiple?: boolean; disabled?: boolean; size?: 'sm' | 'md' }>(), {
  label: 'Attach',
  multiple: true,
  disabled: false,
  size: 'md',
})
const emit = defineEmits<{ files: [files: File[]] }>()
const input = ref<HTMLInputElement | null>(null)

function onChange(e: Event) {
  const el = e.target as HTMLInputElement
  const files = Array.from(el.files ?? [])
  el.value = ''
  if (files.length) emit('files', files)
}
</script>

<template>
  <BaseButton :size="size" :disabled="disabled" @click="input?.click()">
    <template #icon><IconPaperclip :size="16" stroke="1.75" aria-hidden="true" /></template>
    {{ label }}
  </BaseButton>
  <input ref="input" type="file" class="sr-only" tabindex="-1" aria-hidden="true" :multiple="multiple" @change="onChange" />
</template>
