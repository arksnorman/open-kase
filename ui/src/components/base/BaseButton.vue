<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import type { RouteLocationRaw } from 'vue-router'
import Spinner from './Spinner.vue'

const props = withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'strong' | 'ghost' | 'danger'
    size?: 'sm' | 'md' | 'lg'
    type?: 'button' | 'submit' | 'reset'
    loading?: boolean
    disabled?: boolean
    block?: boolean
    /** Renders a RouterLink styled as a button. */
    to?: RouteLocationRaw
  }>(),
  { variant: 'secondary', size: 'md', type: 'button', loading: false, disabled: false, block: false },
)

const classes = computed(() => [
  props.variant === 'primary' ? 'lg-btn-primary' : props.variant === 'strong' ? 'lg-btn-strong' : 'lg-btn',
  props.variant === 'primary' ? '' : 'lg-hov',
  props.variant === 'ghost' ? 'border-transparent bg-transparent' : '',
  props.variant === 'danger' ? 'text-danger' : '',
  props.size === 'sm' ? '!h-7 !px-2.5 !text-xs' : props.size === 'lg' ? '!h-10 !text-[14px]' : '',
  props.block ? 'w-full justify-center' : '',
  props.to ? 'lg-link' : '',
])

// A busy button is disabled, which drops keyboard focus to <body> (the page start). When the
// work ends and focus is still lost, give it back to the button so keyboard and screen-reader
// users continue where they were (e.g. after a failed sign-in or save).
const el = ref<HTMLButtonElement | null>(null)
let hadFocus = false
watch(
  () => props.loading,
  (busy, was) => {
    if (busy && !was) {
      hadFocus = el.value !== null && document.activeElement === el.value
      return
    }
    if (!busy && was && hadFocus) {
      hadFocus = false
      void nextTick(() => {
        const b = el.value
        const lost = document.activeElement === null || document.activeElement === document.body
        if (b?.isConnected && !b.disabled && lost) b.focus({ preventScroll: true })
      })
    }
  },
)
</script>

<template>
  <RouterLink v-if="to && !disabled" :to="to" :class="classes">
    <slot name="icon" />
    <slot />
  </RouterLink>
  <button v-else ref="el" :type="type" :class="classes" :disabled="disabled || loading" :aria-busy="loading || undefined">
    <Spinner v-if="loading" :size="14" />
    <slot v-else name="icon" />
    <slot />
  </button>
</template>
