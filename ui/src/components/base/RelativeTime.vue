<script setup lang="ts">
// Every date in the UI: shows "12 min ago" / "29d ago" (or "in 18 min" for a future time) in the
// body font with tabular digits (`mono` only makes it smaller, kept for existing callers); clicking toggles
// the full local date-time. `pastPrefix`/`futurePrefix` turn it into a deadline phrase such as
// "Breached 4 min ago" / "Due in 18 min" (SLA chips). `dateOnly` is for calendar days
// ("YYYY-MM-DD": report ranges, holidays): day-level distance, and the full date has no time.
// `hint` adds a line to the tooltip after the full date (e.g. "Version 3").
import { computed, ref } from 'vue'
import { useNow } from '@/composables/useNow'
import { daysAgo, fullDateTime, fullDay, timeAgo } from '@/format'

const props = withDefaults(
  defineProps<{ iso: string; mono?: boolean; pastPrefix?: string; futurePrefix?: string; dateOnly?: boolean; hint?: string }>(),
  { mono: false, pastPrefix: '', futurePrefix: '', dateOnly: false },
)
const now = useNow()
// The shared ticker moves once a minute, so on its own it can be older than the timestamp shown:
// a draft saved or a reply sent after the last tick would read "in a moment". The ticker only
// triggers the re-evaluation; the comparison uses the real clock.
const clock = () => Math.max(now.value, Date.now())
const expanded = ref(false)
const full = computed(() => (props.dateOnly ? fullDay(props.iso) : fullDateTime(props.iso)))
const future = computed(() => (props.dateOnly ? daysAgo(props.iso, clock()).startsWith('in ') : Date.parse(props.iso) > clock()))
const rel = computed(() => {
  const text = props.dateOnly ? daysAgo(props.iso, clock()) : timeAgo(props.iso, clock())
  const prefix = future.value ? props.futurePrefix : props.pastPrefix
  return prefix ? `${prefix} ${text}` : text
})
const fullText = computed(() => {
  const prefix = future.value ? props.futurePrefix : props.pastPrefix
  return prefix ? `${prefix} ${full.value}` : full.value
})
</script>

<template>
  <button
    type="button"
    class="inline cursor-pointer rounded-sm border-0 bg-transparent p-0 text-inherit [font-weight:inherit] hover:underline"
    :class="['lg-time', mono ? 'text-xs' : '']"
    v-tooltip="hint ? `${full} · ${hint}` : full"
    :aria-label="expanded ? `${fullText}. Show relative time` : `${rel}, ${full}${hint ? `, ${hint}` : ''}. Show full date`"
    :aria-pressed="expanded"
    @click.stop="expanded = !expanded"
  >
    <time :datetime="iso">{{ expanded ? fullText : rel }}</time>
  </button>
</template>
