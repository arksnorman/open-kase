<script setup lang="ts">
/** `titleTag`: `h1` when the empty state is the whole page (no projects, not found). */
withDefaults(defineProps<{ title: string; description?: string; titleTag?: 'p' | 'h1' | 'h2' }>(), { titleTag: 'p' })
</script>

<template>
  <div class="flex flex-col items-start gap-1.5 px-4 py-6">
    <span v-if="$slots.icon" class="text-muted" aria-hidden="true"><slot name="icon" /></span>
    <component :is="titleTag" class="m-0 font-semibold">{{ title }}</component>
    <p v-if="description" class="m-0 text-muted">{{ description }}</p>
    <div v-if="$slots.actions" class="flex gap-2 pt-1"><slot name="actions" /></div>
  </div>
</template>
