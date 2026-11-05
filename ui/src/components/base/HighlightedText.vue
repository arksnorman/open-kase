<script setup lang="ts">
// Plain text with highlighted ranges (search snippets). Ranges are UTF-16 code-unit offsets,
// i.e. JS string indices; every part renders as a text node, never as HTML.
import { computed } from 'vue'

const props = defineProps<{ text: string; ranges: { start: number; end: number }[] }>()

const parts = computed(() => {
  const len = props.text.length
  const sorted = props.ranges
    .map((r) => ({ start: Math.max(0, Math.min(len, Math.trunc(r.start))), end: Math.max(0, Math.min(len, Math.trunc(r.end))) }))
    .filter((r) => r.end > r.start)
    .sort((a, b) => a.start - b.start)
  const out: { text: string; mark: boolean }[] = []
  let pos = 0
  for (const r of sorted) {
    const start = Math.max(r.start, pos)
    if (r.end <= start) continue
    if (start > pos) out.push({ text: props.text.slice(pos, start), mark: false })
    out.push({ text: props.text.slice(start, r.end), mark: true })
    pos = r.end
  }
  if (pos < len) out.push({ text: props.text.slice(pos), mark: false })
  return out
})
</script>

<template>
  <span><template v-for="(p, i) in parts" :key="i"><mark v-if="p.mark" class="lg-mark">{{ p.text }}</mark><template v-else>{{ p.text }}</template></template></span>
</template>
