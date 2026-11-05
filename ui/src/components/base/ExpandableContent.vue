<script setup lang="ts">
// Long content, clamped: when the slot is taller than `maxHeight` (plus a small tolerance, so a
// line or two never gets hidden behind a toggle) it is cut with a soft fade and a "Show more" /
// "Show less" toggle. Short content renders as is with no control. Height is re-measured with a
// ResizeObserver (window width, images and late-rendered Markdown). `v-model:expanded` lets the
// caller open it (e.g. a cited message); `overflow` reports whether the content is clamped.
// `tolerance` is the extra height allowed before clamping; `disabled` never clamps (the newest message).
import { computed, onBeforeUnmount, onMounted, ref, useId, watch } from 'vue'
import LinkButton from './LinkButton.vue'

const props = withDefaults(defineProps<{ maxHeight?: number; tolerance?: number; disabled?: boolean; moreLabel?: string; lessLabel?: string }>(), {
  maxHeight: 320,
  tolerance: 48,
  disabled: false,
  moreLabel: 'Show more',
  lessLabel: 'Show less',
})
const expanded = defineModel<boolean>('expanded', { default: false })
const emit = defineEmits<{ overflow: [overflowing: boolean] }>()

const uid = useId()
const contentId = `${uid}-content`
const outer = ref<HTMLElement | null>(null)
const inner = ref<HTMLElement | null>(null)
const overflowing = ref(false)
/** Clamp only when the extra height is worth a toggle (`tolerance`). */
function measure() {
  const h = inner.value?.scrollHeight ?? 0
  overflowing.value = !props.disabled && h > props.maxHeight + props.tolerance
}
watch(overflowing, (v) => emit('overflow', v))
watch(() => [props.maxHeight, props.disabled], measure)

let ro: ResizeObserver | null = null
onMounted(() => {
  measure()
  if (typeof ResizeObserver !== 'undefined' && inner.value) {
    ro = new ResizeObserver(measure)
    ro.observe(inner.value)
  }
})
onBeforeUnmount(() => ro?.disconnect())

const clamped = computed(() => overflowing.value && !expanded.value)
const clampStyle = computed(() =>
  clamped.value
    ? {
        maxHeight: `${props.maxHeight}px`,
        overflow: 'hidden',
        maskImage: 'linear-gradient(to bottom, #000 calc(100% - 44px), transparent)',
        WebkitMaskImage: 'linear-gradient(to bottom, #000 calc(100% - 44px), transparent)',
      }
    : undefined,
)

function toggle() {
  const collapsing = expanded.value
  expanded.value = !expanded.value
  if (collapsing) {
    // Keep the reader oriented: bring the (now short) content back into view if its top scrolled away.
    requestAnimationFrame(() => {
      const el = outer.value
      if (!el || el.getBoundingClientRect().top >= 0) return
      const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
      el.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' })
    })
  }
}
</script>

<template>
  <div ref="outer" class="flex min-w-0 flex-col gap-1.5">
    <div :id="contentId" :style="clampStyle">
      <div ref="inner" class="flow-root"><slot /></div>
    </div>
    <LinkButton
      v-if="overflowing"
      tone="muted"
      class="self-start text-xs font-medium"
      :aria-expanded="expanded ? 'true' : 'false'"
      :aria-controls="contentId"
      @click="toggle"
    >
      {{ expanded ? lessLabel : moreLabel }}
    </LinkButton>
  </div>
</template>
