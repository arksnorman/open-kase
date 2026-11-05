<script setup lang="ts">
// Text-styled button for inline actions ("Assign…", evidence references, "Use a recovery code").
// `tone="muted"` is the quieter secondary-action style used under forms.
withDefaults(defineProps<{ type?: 'button' | 'submit'; tone?: 'default' | 'muted'; disabled?: boolean }>(), { type: 'button', tone: 'default' })
</script>

<template>
  <button
    :type="type"
    :disabled="disabled"
    class="inline cursor-pointer rounded-sm border-0 bg-transparent p-0 text-left [font:inherit] underline-offset-2 hover:underline disabled:opacity-50"
    :class="tone === 'muted' ? 'text-muted hover:text-text' : 'text-inherit'"
  >
    <slot />
  </button>
</template>
