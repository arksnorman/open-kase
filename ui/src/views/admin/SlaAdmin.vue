<script setup lang="ts">
// SLA (admins): on/off, a first-response target per priority (wall-clock minutes, 24/7) and the
// due-soon share. Turning it off stops the running clocks of open tickets; changes apply to new
// tickets. GET /sla returns version 0 (off, no targets) until the first save.
import { computed, reactive, ref, watch } from 'vue'
import { slaApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Priority, SLAInput, SLASettings } from '@/api/types'
import {
  BaseAlert,
  BaseBadge,
  BaseButton,
  BaseCard,
  DurationInput,
  ErrorState,
  LinkButton,
  LoadingState,
  PriorityBadge,
  RelativeTime,
  SwitchInput,
} from '@/components/base'
import { useQuery } from '@/composables/useQuery'
import { priorityShort } from '@/format'
import AdminPage from './AdminPage.vue'

const PRIOS = ['1', '2', '3', '4'] as const

/** Suggested targets shown before the first save (not stored until saved). */
const SUGGESTED: SLAInput['targets'] = {
  '1': { firstResponseMinutes: 30 },
  '2': { firstResponseMinutes: 120 },
  '3': { firstResponseMinutes: 480 },
  '4': { firstResponseMinutes: 1440 },
}

function toInput(s: SLASettings): SLAInput {
  const targets = JSON.parse(JSON.stringify(s.version === 0 ? SUGGESTED : s.targets)) as SLAInput['targets']
  return { active: s.active, warningPercent: s.warningPercent, targets }
}

const slaQ = useQuery(
  () => 'sla',
  (signal) => slaApi.get({ signal }),
)
const sla = computed(() => slaQ.data.value ?? null)

const form = reactive<SLAInput>({ active: false, warningPercent: 80, targets: JSON.parse(JSON.stringify(SUGGESTED)) })
/** Version the next save replaces (raised after a conflict, so the admin overwrites knowingly). */
const baseVersion = ref(0)
/** The form as loaded, to tell whether there are unsaved changes. */
const loaded = ref('')
function load(s: SLASettings) {
  Object.assign(form, toInput(s))
  baseVersion.value = s.version
  loaded.value = JSON.stringify(toInput(s))
}
watch(
  () => slaQ.data.value,
  (s) => {
    if (s) load(s)
  },
  { immediate: true },
)
const dirty = computed(() => JSON.stringify(form) !== loaded.value)

function minutes(p: (typeof PRIOS)[number]): number | null {
  return form.targets[p].firstResponseMinutes ?? null
}
function invalidTarget(p: (typeof PRIOS)[number]): boolean {
  const v = minutes(p)
  return v !== null && !(Number.isInteger(v) && v > 0)
}

/** "24 min", "1h 36m", "19h 12m" (whole minutes, rounded down like the server's warning time). */
function shortDuration(totalMinutes: number): string {
  const m = Math.floor(totalMinutes)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  const rest = m % 60
  return rest ? `${h}h ${rest}m` : `${h}h`
}
function dueSoonAfter(p: (typeof PRIOS)[number]): string {
  return shortDuration(((minutes(p) ?? 0) * form.warningPercent) / 100)
}

const localErrors = computed(() => {
  const out: string[] = []
  if (!(form.warningPercent >= 50 && form.warningPercent <= 95)) out.push('The due-soon share must be between 50 and 95 %.')
  for (const p of PRIOS) if (invalidTarget(p)) out.push(`P${p}: enter a positive number of minutes or hours (whole minutes).`)
  return out
})

const saving = ref(false)
const error = ref<unknown>(null)
const conflict = ref(false)
const saved = ref(false)
async function save() {
  if (localErrors.value.length) return
  saving.value = true
  error.value = null
  saved.value = false
  try {
    const result = await slaApi.put({ ...form }, baseVersion.value)
    slaQ.set(result)
    conflict.value = false
    saved.value = true
  } catch (err) {
    if (err instanceof ApiError && (err.is('versionConflict') || err.status === 428)) {
      // Keep the admin's form; the next save replaces the newer version knowingly.
      const keep = JSON.parse(JSON.stringify(form)) as SLAInput
      await slaQ.refetch()
      Object.assign(form, keep)
      conflict.value = true
    } else error.value = err
  } finally {
    saving.value = false
  }
}
function reset() {
  if (sla.value) load(sla.value)
  conflict.value = false
  error.value = null
  saved.value = false
}

const fieldErrors = computed(() => (error.value instanceof ApiError ? error.value.fieldErrors : []))
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage title="SLA" description="How quickly staff must send the first public reply, by priority. Counted around the clock. Changes apply to new tickets.">
      <template v-if="sla" #actions>
        <span v-if="sla.updatedAt" class="text-xs text-muted">
          Updated <RelativeTime :iso="sla.updatedAt" class="lg-hover-hint" :hint="`Version ${sla.version} · tickets keep the targets they started with`" />
        </span>
        <span v-else class="text-xs text-muted">Not saved yet</span>
      </template>

      <BaseCard v-if="slaQ.error.value" as="div" size="sm" class="!py-0">
        <ErrorState :error="slaQ.error.value" title="Could not load the SLA" :retrying="slaQ.loading.value" @retry="slaQ.refetch" />
      </BaseCard>
      <LoadingState v-if="slaQ.loading.value && !sla" />
      <form v-else-if="sla" class="flex max-w-3xl flex-col gap-4" novalidate @submit.prevent="save">
        <BaseCard as="div">
          <div class="flex items-start gap-3.5">
            <SwitchInput v-model="form.active" class="mt-0.5" labelledby="sla-on-label" describedby="sla-on-hint" />
            <div class="flex min-w-0 flex-col gap-0.5">
              <div class="flex flex-wrap items-center gap-2">
                <strong id="sla-on-label" class="text-sm font-semibold">{{ form.active ? 'SLA is on' : 'SLA is off' }}</strong>
                <BaseBadge v-if="form.active" tone="success">On</BaseBadge>
              </div>
              <span id="sla-on-hint" class="text-muted">{{
                form.active
                  ? 'New tickets get a first-response deadline. Turning it off stops the running clocks; recorded misses stay.'
                  : 'New tickets get no deadline. Turning it on applies to new tickets only.'
              }}</span>
            </div>
          </div>
        </BaseCard>

        <BaseCard title="First-response targets">
          <p v-if="sla.version === 0" class="m-0 text-xs text-muted">Suggested targets; nothing is stored until you save.</p>
          <ul class="m-0 flex list-none flex-col p-0">
            <li
              v-for="(p, i) in PRIOS"
              :key="p"
              class="grid grid-cols-[150px_auto_minmax(0,1fr)] items-center gap-3.5 py-2.5 max-sm:grid-cols-1 max-sm:gap-1.5"
              :class="i > 0 ? 'border-t border-line' : ''"
            >
              <span><PriorityBadge :priority="Number(p) as Priority" /></span>
              <DurationInput v-model="form.targets[p].firstResponseMinutes" :label="`P${p} first-response target`" :invalid="invalidTarget(p)" />
              <span v-if="invalidTarget(p)" class="text-xs text-danger">Enter a positive number</span>
              <span v-else-if="minutes(p) === null" class="text-xs italic text-muted">No target: {{ priorityShort(Number(p) as 1 | 2 | 3 | 4) }}-priority tickets get no SLA</span>
              <span v-else class="text-xs text-muted" aria-live="polite">Due soon after <strong class="font-semibold text-warn">{{ dueSoonAfter(p) }}</strong></span>
            </li>
          </ul>
          <p class="m-0 text-xs text-muted">
            Only the first public reply by staff counts; agent notes don’t. Reopened tickets get no new deadline. Leave a box empty for no target.
          </p>
        </BaseCard>

        <BaseCard title="Due-soon warning">
          <div class="flex flex-wrap items-center gap-2.5">
            <label for="sla-warning">Show “due soon” when</label>
            <input
              id="sla-warning"
              v-model.number="form.warningPercent"
              type="range"
              min="50"
              max="95"
              step="1"
              class="w-[220px] max-w-full accent-[var(--kase-accent)]"
              :aria-valuetext="`${form.warningPercent}% of the target has passed`"
              aria-describedby="sla-warning-tail"
            />
            <span class="min-w-[3ch] font-semibold tabular-nums" aria-hidden="true">{{ form.warningPercent }}%</span>
            <span id="sla-warning-tail">of the target has passed</span>
          </div>
          <p class="m-0 text-xs text-muted">Due-soon tickets turn amber and appear in the Due soon queue.</p>
        </BaseCard>

        <BaseAlert v-if="localErrors.length" tone="warn">
          <ul class="m-0 pl-4"><li v-for="e in localErrors" :key="e">{{ e }}</li></ul>
        </BaseAlert>
        <BaseAlert v-if="conflict" tone="warn">
          The SLA was changed by someone else while you edited it. Your edits are kept; saving now replaces their version.
          <LinkButton class="underline" @click="reset">Discard my edits and load theirs</LinkButton>
        </BaseAlert>
        <BaseAlert v-if="error && !fieldErrors.length" :error="error" />
        <BaseAlert v-if="fieldErrors.length">
          <ul class="m-0 pl-4"><li v-for="e in fieldErrors" :key="e.field + e.code"><span class="lg-mono text-xs">{{ e.field }}</span>: {{ e.message }}</li></ul>
        </BaseAlert>
        <div class="flex flex-wrap items-center gap-3">
          <BaseButton type="submit" variant="primary" :loading="saving" :disabled="localErrors.length > 0">Save</BaseButton>
          <BaseButton :disabled="!dirty && !conflict" @click="reset">Reset</BaseButton>
          <span v-if="saved && !dirty" role="status" class="text-xs text-muted">{{ sla.active ? 'Saved.' : 'Saved. SLA is off.' }}</span>
          <span v-else-if="dirty" class="text-xs text-muted">Unsaved changes</span>
        </div>
      </form>
    </AdminPage>
  </div>
</template>
