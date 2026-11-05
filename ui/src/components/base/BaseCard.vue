<script setup lang="ts">
// Bordered panel. `size` sets padding/gap (sm: dense tiles, lg: auth forms); `padded=false`
// is for tables/lists that run edge to edge (`scroll` lets wide tables scroll horizontally).
// `tone="subtle"` is a recessed info block; `tone="note"` marks agent-only content (amber).
// A scrolling card is `relative` so screen-reader-only labels inside it (absolutely positioned) stay in its scroll box instead of widening the page.
withDefaults(
  defineProps<{ title?: string; padded?: boolean; as?: string; size?: 'sm' | 'md' | 'lg'; tone?: 'default' | 'subtle' | 'note'; scroll?: boolean }>(),
  { padded: true, as: 'section', size: 'md', tone: 'default', scroll: false },
)
</script>

<template>
  <component
    :is="as"
    class="lg-panel"
    :class="[
      padded ? ['flex flex-col', size === 'sm' ? 'gap-2 p-3' : size === 'lg' ? 'gap-4 p-6' : 'gap-3 p-4'] : scroll ? 'relative overflow-x-auto' : 'overflow-hidden',
      tone === 'subtle' ? '!bg-subtle' : tone === 'note' ? 'lg-msg-note' : '',
    ]"
  >
    <h2 v-if="title" class="lg-h3">{{ title }}</h2>
    <slot />
  </component>
</template>
