<script setup lang="ts">
// A full-width selectable row inside a list (radio-style pickers, master/detail lists).
// The caller supplies the ARIA pattern via attributes (role="radio" + aria-checked, or
// aria-current); `selected` only drives the visual state.
withDefaults(defineProps<{ selected?: boolean; variant?: 'bordered' | 'plain' }>(), { selected: false, variant: 'bordered' })
</script>

<template>
  <button
    type="button"
    class="lg-hov flex w-full items-center gap-2 rounded-md text-left"
    :class="[
      variant === 'bordered' ? 'border px-2.5 py-1.5' : 'lg-nav border-0',
      variant === 'bordered' ? (selected ? 'border-border-strong bg-sel' : 'border-border bg-surface') : selected ? '!bg-subtle font-semibold' : 'bg-transparent',
    ]"
  >
    <slot />
  </button>
</template>
