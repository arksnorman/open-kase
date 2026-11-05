<script setup lang="ts">
// Reports (admins). The API computes them on request with a
// bounded live traversal; they are not stored snapshots, which the page says plainly.
// Visuals are minimal ink bars inside accessible tables (every value is also printed).
import { computed, ref } from 'vue'
import { reportsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { ReportSummary } from '@/api/types'
import { BaseAlert, BaseButton, BaseCard, CheckboxInput, ErrorState, FormField, LoadingState, RelativeTime, StatusBadge, TextInput } from '@/components/base'
import { useQuery } from '@/composables/useQuery'
import { duration, priorityLong } from '@/format'
import AdminPage from './AdminPage.vue'


const from = ref('')
const to = ref('')
const applied = ref<{ from?: string; to?: string }>({})
const reportQ = useQuery(
  () => `reports:${applied.value.from ?? ''}:${applied.value.to ?? ''}`,
  (signal) => reportsApi.summary(applied.value, { signal }),
)
const r = computed<ReportSummary | undefined>(() => reportQ.data.value)

function apply() {
  applied.value = { ...(from.value ? { from: from.value } : {}), ...(to.value ? { to: to.value } : {}) }
}
function reset() {
  from.value = to.value = ''
  applied.value = {}
}
const forbidden = computed(() => reportQ.error.value instanceof ApiError && reportQ.error.value.status === 403)
const rangeError = computed(() => (reportQ.error.value instanceof ApiError && reportQ.error.value.status === 422 ? reportQ.error.value : null))
/** Any other failure replaces the figures and tables (a cached report would read as current). */
const loadError = computed(() => (reportQ.error.value && !forbidden.value && !rangeError.value ? reportQ.error.value : null))

function pct(n: number, max: number): string {
  return max > 0 ? `${Math.max(n > 0 ? 2 : 0, Math.round((n / max) * 100))}%` : '0%'
}
const statusMax = computed(() => Math.max(0, ...(r.value?.backlog.byStatus.map((s) => s.count) ?? [])))
const prioMax = computed(() => Math.max(0, ...(r.value?.backlog.byPriority.map((s) => s.count) ?? [])))
const groupMax = computed(() => Math.max(0, ...(r.value?.backlog.byGroup.map((s) => s.count) ?? [])))

const hideEmptyDays = ref(true)
const days = computed(() => {
  const all = r.value?.createdVsResolved ?? []
  return hideEmptyDays.value ? all.filter((d) => d.created || d.resolved) : all
})
const dayMax = computed(() => Math.max(0, ...(r.value?.createdVsResolved.flatMap((d) => [d.created, d.resolved]) ?? [])))
const totals = computed(() =>
  (r.value?.createdVsResolved ?? []).reduce((a, d) => ({ created: a.created + d.created, resolved: a.resolved + d.resolved }), { created: 0, resolved: 0 }),
)
function median(m: { medianSeconds: number | null }): string {
  return m.medianSeconds === null ? '—' : duration(m.medianSeconds)
}
const reopenRate = computed(() => (r.value?.reopen.rate == null ? '—' : `${Math.round(r.value.reopen.rate * 1000) / 10} %`))
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage title="Reports" description="Backlog now, plus flow, response times, SLA breaches and reopens for a date range (UTC days, at most 92).">
      <form class="flex flex-wrap items-end gap-3" novalidate @submit.prevent="apply">
        <FormField label="From" optional>
          <TextInput v-model="from" type="date" size="sm" class="!w-44" />
        </FormField>
        <FormField label="To" optional>
          <TextInput v-model="to" type="date" size="sm" class="!w-44" />
        </FormField>
        <BaseButton type="submit" variant="primary" class="!h-[34px]" :loading="reportQ.loading.value">Show report</BaseButton>
        <BaseButton v-if="applied.from || applied.to" class="!h-[34px]" @click="reset">Last 30 days</BaseButton>
      </form>

      <BaseAlert v-if="forbidden" error="Reports are available to admins." />
      <BaseAlert v-else-if="rangeError" :error="rangeError.fieldErrors.map((e) => e.message).join(' ') || rangeError" />
      <BaseCard v-else-if="loadError" as="div" size="sm" class="!py-0">
        <ErrorState :error="loadError" title="Could not compute the report" :retrying="reportQ.loading.value" @retry="reportQ.refetch" />
      </BaseCard>
      <LoadingState v-if="reportQ.loading.value && !r" text="Computing report…" />

      <template v-if="r && !loadError">
        <BaseAlert tone="info">
          <strong class="font-semibold">Bounded live traversal</strong>, computed on request <RelativeTime :iso="r.generatedAt" /> for
          <RelativeTime :iso="r.range.from" date-only /> to <RelativeTime :iso="r.range.to" date-only /> ({{ r.range.timezone }}). Not a stored point-in-time snapshot: figures can change as tickets change.
          {{ r.bounds.note }} At most {{ r.bounds.maxSamples.toLocaleString() }} tickets are sampled per median.
        </BaseAlert>

        <dl class="m-0 grid grid-cols-[repeat(auto-fill,minmax(180px,1fr))] gap-3" aria-label="Key figures">
          <BaseCard as="div" size="sm">
            <dt class="text-xs text-muted">Backlog now</dt>
            <dd class="m-0 text-2xl font-semibold">{{ r.backlog.total.toLocaleString() }}</dd>
            <dd class="m-0 text-xs text-muted">open, in progress and waiting</dd>
          </BaseCard>
          <BaseCard as="div" size="sm">
            <dt class="text-xs text-muted">Median first response</dt>
            <dd class="m-0 text-2xl font-semibold">{{ median(r.firstResponse) }}</dd>
            <dd class="m-0 text-xs text-muted">{{ r.firstResponse.sampleSize }} tickets created in range<template v-if="r.firstResponse.truncated"> · sample truncated</template></dd>
          </BaseCard>
          <BaseCard as="div" size="sm">
            <dt class="text-xs text-muted">Median resolution</dt>
            <dd class="m-0 text-2xl font-semibold">{{ median(r.resolution) }}</dd>
            <dd class="m-0 text-xs text-muted">{{ r.resolution.sampleSize }} tickets resolved in range<template v-if="r.resolution.truncated"> · sample truncated</template></dd>
          </BaseCard>
          <BaseCard as="div" size="sm">
            <dt class="text-xs text-muted">SLA breaches</dt>
            <dd class="m-0 text-2xl font-semibold" :class="r.slaBreaches.firstResponse > 0 ? 'text-danger' : ''">{{ r.slaBreaches.firstResponse.toLocaleString() }}</dd>
            <dd class="m-0 text-xs text-muted">First-response deadlines missed in range</dd>
          </BaseCard>
          <BaseCard as="div" size="sm">
            <dt class="text-xs text-muted">Reopen rate</dt>
            <dd class="m-0 text-2xl font-semibold">{{ reopenRate }}</dd>
            <dd class="m-0 text-xs text-muted">{{ r.reopen.reopened }} reopened of {{ r.reopen.resolved }} resolved</dd>
          </BaseCard>
        </dl>
        <p class="m-0 text-xs text-muted">Medians are wall-clock durations.</p>

        <div class="grid grid-cols-[repeat(auto-fit,minmax(300px,1fr))] gap-4">
          <BaseCard title="Backlog by status">
            <table class="lg-table" aria-label="Backlog by status">
              <thead><tr><th scope="col">Status</th><th scope="col" class="w-1/2">Tickets</th></tr></thead>
              <tbody>
                <tr v-for="s in r.backlog.byStatus" :key="s.id">
                  <th scope="row" class="text-left font-normal"><StatusBadge :status="s" /></th>
                  <td><span class="rp-row"><span class="rp-track"><span class="rp-bar" :style="{ width: pct(s.count, statusMax) }"></span></span><span class="rp-val">{{ s.count }}</span></span></td>
                </tr>
                <tr v-if="!r.backlog.byStatus.length"><td colspan="2" class="text-muted">No open tickets.</td></tr>
              </tbody>
            </table>
          </BaseCard>
          <BaseCard title="Backlog by priority">
            <table class="lg-table" aria-label="Backlog by priority">
              <thead><tr><th scope="col">Priority</th><th scope="col" class="w-1/2">Tickets</th></tr></thead>
              <tbody>
                <tr v-for="p in r.backlog.byPriority" :key="p.priority">
                  <th scope="row" class="text-left font-normal">{{ priorityLong(p.priority) }}</th>
                  <td><span class="rp-row"><span class="rp-track"><span class="rp-bar" :style="{ width: pct(p.count, prioMax) }"></span></span><span class="rp-val">{{ p.count }}</span></span></td>
                </tr>
                <tr v-if="!r.backlog.byPriority.length"><td colspan="2" class="text-muted">No open tickets.</td></tr>
              </tbody>
            </table>
          </BaseCard>
          <BaseCard title="Backlog by group">
            <table class="lg-table" aria-label="Backlog by group">
              <thead><tr><th scope="col">Group</th><th scope="col" class="w-1/2">Tickets</th></tr></thead>
              <tbody>
                <tr v-for="g in r.backlog.byGroup" :key="g.id">
                  <th scope="row" class="text-left font-normal">{{ g.name }}</th>
                  <td><span class="rp-row"><span class="rp-track"><span class="rp-bar" :style="{ width: pct(g.count, groupMax) }"></span></span><span class="rp-val">{{ g.count }}</span></span></td>
                </tr>
                <tr v-if="!r.backlog.byGroup.length"><td colspan="2" class="text-muted">No open tickets.</td></tr>
              </tbody>
            </table>
          </BaseCard>
        </div>

        <BaseCard title="Created vs resolved per day">
          <div class="flex flex-wrap items-center gap-4">
            <span class="flex items-center gap-1.5 text-xs" aria-hidden="true"><span class="rp-key rp-bar"></span> Created ({{ totals.created }})</span>
            <span class="flex items-center gap-1.5 text-xs" aria-hidden="true"><span class="rp-key rp-bar-alt"></span> Resolved ({{ totals.resolved }})</span>
            <span class="sr-only">Totals: {{ totals.created }} created, {{ totals.resolved }} resolved.</span>
            <span class="flex-1"></span>
            <CheckboxInput v-model="hideEmptyDays" label="Hide days without activity" />
          </div>
          <p class="m-0 text-xs text-muted">Created counts tickets created that day; resolved counts resolve events that day (UTC). Both bars share one scale.</p>
          <div class="overflow-x-auto">
            <table class="lg-table" aria-label="Created and resolved tickets per day">
              <thead>
                <tr><th scope="col" class="w-36">Day (UTC)</th><th scope="col">Created</th><th scope="col">Resolved</th></tr>
              </thead>
              <tbody>
                <tr v-for="d in days" :key="d.date">
                  <th scope="row" class="text-left font-normal"><RelativeTime :iso="d.date" date-only /></th>
                  <td><span class="rp-row"><span class="rp-track"><span class="rp-bar" :style="{ width: pct(d.created, dayMax) }"></span></span><span class="rp-val">{{ d.created }}</span></span></td>
                  <td><span class="rp-row"><span class="rp-track"><span class="rp-bar-alt" :style="{ width: pct(d.resolved, dayMax) }"></span></span><span class="rp-val">{{ d.resolved }}</span></span></td>
                </tr>
                <tr v-if="!days.length"><td colspan="3" class="text-muted">No tickets created or resolved in this range.</td></tr>
              </tbody>
            </table>
          </div>
        </BaseCard>
      </template>
    </AdminPage>
  </div>
</template>

<style scoped>
/* Minimal bars: ink marks on a hairline track, 8px thick, 4px rounded data end, square baseline. */
.rp-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.rp-track {
  flex: 1;
  height: 8px;
  min-width: 40px;
  border-bottom: 1px solid var(--kase-line);
}
.rp-bar,
.rp-bar-alt {
  display: block;
  height: 8px;
  border-radius: 0 4px 4px 0;
  background: var(--kase-text);
}
.rp-bar-alt {
  background: var(--kase-muted);
}
.rp-key {
  width: 12px;
  height: 8px;
  border-radius: 0 4px 4px 0;
}
.rp-val {
  min-width: 2.5ch;
  text-align: right;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
@media (forced-colors: active) {
  .rp-bar {
    background: CanvasText;
  }
  .rp-bar-alt {
    background: GrayText;
  }
}
</style>
