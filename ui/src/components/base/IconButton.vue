<script setup lang="ts">
// Icon-only button: `label` is required and becomes the accessible name.
withDefaults(
  defineProps<{ label: string; /** Tooltip text when it says more than the label. */ tip?: string; variant?: 'ghost' | 'bordered'; pressed?: boolean; expanded?: boolean; disabled?: boolean; type?: 'button' | 'submit' }>(),
  { variant: 'ghost', type: 'button', pressed: undefined, expanded: undefined },
)
</script>

<template>
  <button
    :type="type"
    :class="[variant === 'bordered' ? 'lg-icon-btn-b' : 'lg-icon-btn', 'lg-hov']"
    :aria-label="label"
    v-tooltip="tip ?? label"
    :aria-pressed="pressed"
    :aria-expanded="expanded"
    :disabled="disabled"
  >
    <span aria-hidden="true" class="inline-flex"><slot /></span>
  </button>
</template>
