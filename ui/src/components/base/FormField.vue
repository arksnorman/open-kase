<script setup lang="ts">
import { computed, provide, useId } from 'vue'
import { FieldKey } from './field'

const props = withDefaults(
  defineProps<{ label: string; hint?: string; error?: string | null; required?: boolean; optional?: boolean; srOnlyLabel?: boolean; id?: string }>(),
  { required: false, optional: false, srOnlyLabel: false },
)
const auto = useId()
const id = props.id ?? `f-${auto}`
const hintId = `${id}-hint`
const errorId = `${id}-error`
const labelId = `${id}-label`

// A <label for> focuses labelable elements only; a div-based combobox (SelectInput) is focused here.
function onLabelClick(e: MouseEvent) {
  const el = document.getElementById(id)
  if (el && el.getAttribute('role') === 'combobox' && !(el instanceof HTMLInputElement)) {
    e.preventDefault()
    el.focus()
  }
}

provide(FieldKey, {
  id,
  labelId,
  describedBy: computed(() => [props.hint ? hintId : '', props.error ? errorId : ''].filter(Boolean).join(' ') || undefined),
  invalid: computed(() => !!props.error),
  required: computed(() => props.required),
})
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <div class="flex items-baseline justify-between gap-2" :class="srOnlyLabel ? 'sr-only' : ''">
      <label :id="labelId" :for="id" class="lg-label" @click="onLabelClick">
        {{ label }}<span v-if="optional" class="font-normal text-muted"> (optional)</span>
      </label>
      <slot name="label-extra" />
    </div>
    <slot />
    <span v-if="hint" :id="hintId" class="text-xs text-muted">{{ hint }}</span>
    <span v-if="error" :id="errorId" class="lg-field-error">{{ error }}</span>
  </div>
</template>
