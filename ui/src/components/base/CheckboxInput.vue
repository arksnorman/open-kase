<script setup lang="ts">
import { useId } from 'vue'

const props = defineProps<{ label: string; hint?: string; id?: string; disabled?: boolean; mono?: boolean }>()
const model = defineModel<boolean>({ default: false })
const auto = useId()
const id = props.id ?? `cb-${auto}`
</script>

<template>
  <div class="flex items-start gap-2">
    <input
      :id="id"
      v-model="model"
      type="checkbox"
      :disabled="disabled"
      class="mt-0.5 size-4 flex-none accent-[var(--kase-primary)]"
      :aria-describedby="hint || $slots.hint ? `${id}-hint` : undefined"
    />
    <div class="flex flex-col">
      <label :for="id" :class="[mono ? 'lg-mono text-xs' : 'text-[13px]', disabled ? 'text-muted' : '']">{{ label }}</label>
      <span v-if="hint || $slots.hint" :id="`${id}-hint`" class="text-xs text-muted"><slot name="hint">{{ hint }}</slot></span>
    </div>
  </div>
</template>
