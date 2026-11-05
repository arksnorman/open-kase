<script setup lang="ts">
// Field-like trigger for popovers and dialogs (project switcher, search launcher). `as="div"`
// renders the same frame without interaction (single-project installs show no switcher).
withDefaults(defineProps<{ size?: 'sm' | 'md'; as?: 'button' | 'div' }>(), { size: 'md', as: 'button' })
</script>

<template>
  <component
    :is="as"
    :type="as === 'button' ? 'button' : undefined"
    class="flex w-full items-center rounded-md border border-border text-left"
    :class="[size === 'sm' ? 'h-8 gap-2 bg-bg px-2.5 text-muted' : 'h-11 gap-2.5 bg-surface px-2.5', as === 'button' ? 'lg-hov' : '']"
  >
    <slot />
  </component>
</template>
