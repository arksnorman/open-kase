<script setup lang="ts">
// SLA chip: "Breached −2h 15m", "25m left", "Met", "Missed" (answered after the deadline);
// the word "SLA" is read by screen readers only and the clock icon marks the chip as the SLA. The
// target, deadline, met/breach times and the SLA version are in the hover tooltip. Colour follows the share of the first-response budget still left: more than half
// neutral, 50–20 % amber, under 20 % or breached red; Met is green, Missed red. Every state carries text and the urgent ones an
// icon, never colour alone. The full deadline is in the tooltip; a click toggles it into the chip.
// The server evaluates `state` at read time; the chip escalates it locally as the minute ticker
// passes `warnAt` / `dueAt`, so an open page does not show a stale state.
import { IconAlarm, IconAlertTriangle, IconCheck, IconClock } from '@tabler/icons-vue'
import { computed, ref } from 'vue'
import type { SLAState, TicketSLA } from '@/api/types'
import { useNow } from '@/composables/useNow'
import { duration, fullDateTime, shortDuration, slaDisplayState, slaKindLabel } from '@/format'

const props = withDefaults(defineProps<{ sla: TicketSLA | null; variant?: 'compact' | 'full' }>(), { variant: 'compact' })
const now = useNow()
const clock = () => Math.max(now.value, Date.now())

const state = computed<SLAState | null>(() => slaDisplayState(props.sla, now.value))
const target = computed(() => props.sla?.targets.find((t) => t.kind === props.sla?.kind) ?? null)
const dueAt = computed(() => props.sla?.dueAt ?? target.value?.dueAt ?? null)

/** Seconds of budget left until the deadline. */
const remaining = computed<number | null>(() => {
  void now.value
  if (!props.sla) return null
  if (dueAt.value) return (Date.parse(dueAt.value) - clock()) / 1000
  return null
})
const overdue = computed(() => (dueAt.value ? Math.max(0, (clock() - Date.parse(dueAt.value)) / 1000) : null))

const tone = computed<'neutral' | 'warn' | 'danger' | 'success'>(() => {
  const st = state.value
  if (st === 'breached') return 'danger'
  if (st === 'met') return earlierBreach.value ? 'danger' : 'success'
  const budget = target.value?.budgetSeconds
  const left = remaining.value
  if (budget && budget > 0 && left !== null) {
    const share = left / budget
    if (share < 0.2) return 'danger'
    if (share <= 0.5) return 'warn'
    return 'neutral'
  }
  if (st === 'dueSoon') return 'warn'
  return 'neutral'
})
const cls = computed(() =>
  tone.value === 'danger' ? 'lg-sla-danger' : tone.value === 'warn' ? 'lg-sla-warn' : tone.value === 'success' ? 'lg-sla-success' : 'lg-sla-plain',
)

/** A deadline of this cycle was missed although the shown state is not breached (e.g. a late first response). */
const earlierBreach = computed(() => !!props.sla?.breached && state.value !== 'breached')

const text = computed(() => {
  const st = state.value
  if (st === 'breached') {
    const o = overdue.value
    return `Breached${o !== null ? ` −${shortDuration(o)}` : ''}`
  }
  if (st === 'met') return earlierBreach.value ? 'Missed' : 'Met'
  const left = remaining.value
  return left !== null ? `${shortDuration(left)} left` : 'Running'
})
/** Read before the visible text, which leaves the word "SLA" out. */
const srLead = 'SLA '

const deadline = computed(() => {
  const s = props.sla
  if (!s) return ''
  const kind = slaKindLabel(s.kind)
  if (state.value === 'met' && target.value?.metAt) return `${kind} met ${fullDateTime(target.value.metAt)}`
  if (dueAt.value) return `${kind} ${state.value === 'breached' ? 'was due' : 'due'} ${fullDateTime(dueAt.value)}`
  return kind
})
/** Hover details: target, deadline, met/breach times, cycle and SLA version. */
const details = computed(() => {
  const s = props.sla
  if (!s) return ''
  const t = target.value
  const parts = [`SLA: ${slaKindLabel(s.kind)}${t?.budgetSeconds ? ` within ${duration(t.budgetSeconds)}` : ''} (24/7)`, deadline.value]
  if (t?.breachedAt && state.value !== 'breached') parts.push(`Deadline missed ${fullDateTime(t.breachedAt)}`)
  else if (s.breached && !t?.breachedAt) parts.push('A deadline of this cycle was missed')
  parts.push(`SLA version ${s.version}`)
  return parts.join('\n')
})
const showDeadline = ref(false)
</script>

<template>
  <span v-if="!sla" class="text-muted"><span aria-hidden="true">—</span><span class="sr-only">No SLA</span></span>
  <span v-else class="inline-flex min-w-0 flex-wrap items-center gap-1.5">
    <button
      type="button"
      class="lg-sla lg-time"
      :class="cls"
      v-tooltip="details"
      :aria-pressed="showDeadline"
      :aria-label="showDeadline ? `${deadline}. Show time left` : `${srLead}${text}${tone === 'warn' ? ' (due soon)' : ''}. ${deadline}. Show deadline`"
      @click.stop="showDeadline = !showDeadline"
    >
      <IconAlertTriangle v-if="state === 'breached' || tone === 'danger'" :size="13" stroke="2" aria-hidden="true" />
      <IconAlarm v-else-if="tone === 'warn'" :size="13" stroke="2" aria-hidden="true" />
      <IconCheck v-else-if="state === 'met'" :size="13" stroke="2" aria-hidden="true" />
      <IconClock v-else :size="13" stroke="2" aria-hidden="true" />
      <span aria-hidden="true" :class="variant === 'full' ? 'lg-hover-hint' : ''">{{ showDeadline ? deadline : text }}</span>
    </button>
  </span>
</template>
