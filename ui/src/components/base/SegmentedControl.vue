<script setup lang="ts" generic="V extends string">
// Radio-group segmented control: arrow keys move and select, Tab leaves the group. `block`
// stretches it to its container with equal segments (e.g. inside a narrow menu).
import { ref } from 'vue'
import type { Segment } from './types'

withDefaults(defineProps<{ options: Segment<V>[]; label: string; variant?: 'strong' | 'quiet'; size?: 'sm' | 'md'; block?: boolean }>(), {
  variant: 'strong',
  size: 'md',
  block: false,
})
const model = defineModel<V>({ required: true })
const buttons = ref<HTMLButtonElement[]>([])

function onKey(e: KeyboardEvent, i: number, options: Segment<V>[]) {
  const delta = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0
  if (!delta) return
  e.preventDefault()
  const next = (i + delta + options.length) % options.length
  const opt = options[next]
  if (opt) model.value = opt.value
  buttons.value[next]?.focus()
}
</script>

<template>
  <div role="radiogroup" :aria-label="label" class="lg-seg" :class="[variant === 'quiet' ? 'lg-seg-quiet bg-surface' : '', block ? '!flex w-full' : '']">
    <button
      v-for="(opt, i) in options"
      :key="opt.value"
      ref="buttons"
      type="button"
      role="radio"
      class="lg-seg-btn lg-hov"
      :class="[size === 'sm' ? '!h-[26px] !px-2 !text-xs' : '', block ? 'min-w-0 flex-1 justify-center' : '']"
      :aria-checked="model === opt.value"
      :data-tone="opt.tone"
      :tabindex="model === opt.value ? 0 : -1"
      @click="model = opt.value"
      @keydown="onKey($event, i, options)"
    >
      <component :is="opt.icon" v-if="opt.icon" :size="size === 'sm' ? 14 : 16" stroke="1.75" aria-hidden="true" />
      {{ opt.label }}
    </button>
  </div>
</template>
