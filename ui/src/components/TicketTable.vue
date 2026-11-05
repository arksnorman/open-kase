<script setup lang="ts">
import { IconInbox } from '@tabler/icons-vue'
// Ticket list on the base DataTable. The layout follows the width the list really gets, so the
// subject always has room and the list never scrolls sideways:
// - wide (≥ 1360px): one line per ticket, nine columns (the original Ledger table);
// - medium (≥ 920px): two lines per ticket, six columns (requester under the subject, group
//   under the assignee, last update under the SLA);
// - compact (below): one stacked entry per ticket.
// `assigneeRedundant` drops the assignee where every row would say the same (Assigned to me,
// Unassigned).
import { holdRemaining, ticketRef } from '@/format'
import { computed, ref } from 'vue'
import type { Ticket } from '@/api/types'
import { BaseAvatar, DataTable, PriorityBadge, RelativeTime, SlaBadge, StatusBadge, type Column } from '@/components/base'
import { useElementWidth } from '@/composables/useElementWidth'

const props = defineProps<{
  tickets: Ticket[]
  label: string
  fromSlug?: string
  loading?: boolean
  /** The list failed to load: rows are replaced by the failure and "Try again" (emits `retry`). */
  error?: unknown
  /** Filters or search narrow the list: empty reads "No matching tickets" with "Clear filters". */
  filtered?: boolean
  hasMore?: boolean
  loadingMore?: boolean
  /** Unfiltered empty state (default "No tickets in this view."). */
  emptyText?: string
  /** Every row has the same assignee (the viewer, or nobody): leave the assignee out. */
  assigneeRedundant?: boolean
}>()
defineEmits<{ retry: []; 'clear-filters': []; 'load-more': [] }>()

/** Status badge tooltip: the hold reason and time left ("Waiting on Engineering · back in 4d 22h"). */
function holdTip(row: Ticket): string | undefined {
  return row.hold ? `${row.hold.reasonName} · back in ${holdRemaining(row.hold.until)}` : undefined
}

const root = ref<HTMLElement | null>(null)
const width = useElementWidth(root)
const tier = computed<'wide' | 'medium' | 'compact'>(() => {
  const w = width.value
  if (w >= 1360) return 'wide'
  // 0: not measured yet (first render); the medium layout is the most common one.
  return w === 0 || w >= 920 ? 'medium' : 'compact'
})

const columns = computed<Column[]>(() => {
  const assignee = !props.assigneeRedundant
  if (tier.value === 'wide')
    return [
      { key: 'key', label: 'Number', width: '104px' },
      { key: 'subject', label: 'Subject' },
      { key: 'requester', label: 'Requester / Org', width: '190px' },
      { key: 'priority', label: 'Priority', width: '104px' },
      { key: 'status', label: 'Status', width: '172px' },
      { key: 'group', label: 'Group', width: '100px' },
      ...(assignee ? [{ key: 'assignee', label: 'Assignee', width: '140px' }] : []),
      { key: 'updated', label: 'Updated', width: '96px' },
      { key: 'sla', label: 'SLA', width: '168px' },
    ]
  if (tier.value === 'medium')
    return [
      { key: 'key', label: 'Number', width: '104px' },
      { key: 'ticket', label: 'Subject / Requester' },
      { key: 'priority', label: 'Priority', width: '104px' },
      { key: 'status', label: 'Status', width: '172px' },
      { key: 'owner', label: assignee ? 'Assignee / Group' : 'Group', width: assignee ? '150px' : '110px' },
      { key: 'due', label: 'SLA / Updated', width: '168px' },
    ]
  return [{ key: 'card', label: 'Ticket' }]
})

function linkTo(t: Ticket) {
  return {
    name: 'ticket',
    params: { ticketNumber: String(t.number) },
    query: props.fromSlug ? { from: props.fromSlug } : {},
  }
}
const requesterName = (t: Ticket) => t.requester.name || t.requester.email
</script>

<template>
  <div ref="root" class="min-w-0">
    <DataTable
      :columns="columns"
      :rows="tickets"
      :row-key="(t) => t.id"
      :row-link="linkTo"
      :label="label"
      :loading="loading"
      :error="error"
      error-title="Could not load tickets"
      :filtered="filtered"
      filtered-text="No matching tickets"
      :has-more="hasMore"
      :loading-more="loadingMore"
      :empty-text="emptyText ?? 'No tickets in this view.'"
      :empty-icon="IconInbox"
      @retry="$emit('retry')"
      @clear-filters="$emit('clear-filters')"
      @load-more="$emit('load-more')"
    >
      <!-- shared -->
      <template #cell-key="{ row }">
        <RouterLink :to="linkTo(row)" class="lg-link lg-mono text-xs font-medium whitespace-nowrap">{{ ticketRef(row) }}</RouterLink>
      </template>
      <template #cell-priority="{ row }"><PriorityBadge :priority="row.priority" /></template>
      <template #cell-status="{ row }">
        <div class="flex min-w-0 flex-col items-start gap-0.5 py-0.5">
          <StatusBadge :status="row.status" :title="holdTip(row)" class="max-w-full" />
          <span v-if="row.hold" class="sr-only">{{ holdTip(row) }}</span>
        </div>
      </template>

      <!-- wide: one line per ticket -->
      <template #cell-subject="{ row }">
        <div class="lg-ell font-medium" v-tooltip.truncated="row.subject">{{ row.subject }}</div>
      </template>
      <template #cell-requester="{ row }">
        <div class="flex min-w-0 flex-col gap-0.5 py-1">
          <span class="flex min-w-0 items-center gap-1.5" v-tooltip.truncated="requesterName(row)">
            <BaseAvatar :name="requesterName(row)" role="requester" size="xs" decorative /><span class="lg-ell">{{ requesterName(row) }}</span>
          </span>
          <span v-if="row.organisation" class="lg-ell text-xs text-muted" v-tooltip.truncated="row.organisation.name">{{ row.organisation.name }}</span>
        </div>
      </template>
      <template #cell-group="{ row }"><div class="lg-ell" v-tooltip.truncated="row.group.name">{{ row.group.name }}</div></template>
      <template #cell-assignee="{ row }">
        <span v-if="!row.assignee" class="text-muted">Unassigned</span>
        <span v-else class="flex min-w-0 items-center gap-1.5" v-tooltip.truncated="row.assignee.name">
          <BaseAvatar :name="row.assignee.name" size="xs" role="staff" decorative /><span class="lg-ell">{{ row.assignee.name }}</span>
        </span>
      </template>
      <template #cell-updated="{ row }"><span class="text-muted"><RelativeTime :iso="row.activityAt" mono /></span></template>
      <template #cell-sla="{ row }"><SlaBadge :sla="row.sla" /></template>

      <!-- medium: two lines per ticket -->
      <template #cell-ticket="{ row }">
        <div class="flex min-w-0 flex-col gap-0.5 py-2">
          <div class="lg-ell font-medium" v-tooltip.truncated="row.subject">{{ row.subject }}</div>
          <div class="flex min-w-0 items-center gap-1.5 text-xs text-muted">
            <BaseAvatar :name="requesterName(row)" role="requester" size="xs" decorative /><span class="lg-ell">{{ requesterName(row) }}</span>
            <span v-if="row.organisation" class="lg-ell flex-none">· {{ row.organisation.name }}</span>
          </div>
        </div>
      </template>
      <template #cell-owner="{ row }">
        <div class="flex min-w-0 flex-col gap-0.5">
          <template v-if="!assigneeRedundant">
            <span v-if="!row.assignee" class="text-muted">Unassigned</span>
            <span v-else class="flex min-w-0 items-center gap-1.5" v-tooltip.truncated="row.assignee.name">
              <BaseAvatar :name="row.assignee.name" size="xs" role="staff" decorative /><span class="lg-ell">{{ row.assignee.name }}</span>
            </span>
          </template>
          <div class="lg-ell" :class="assigneeRedundant ? '' : 'text-xs text-muted'" v-tooltip.truncated="row.group.name">{{ row.group.name }}</div>
        </div>
      </template>
      <template #cell-due="{ row }">
        <div class="flex min-w-0 flex-col items-start gap-0.5">
          <SlaBadge v-if="row.sla" :sla="row.sla" />
          <span class="text-xs text-muted"><span class="sr-only">Updated </span><RelativeTime :iso="row.activityAt" mono /></span>
        </div>
      </template>

      <!-- compact: one stacked entry per ticket -->
      <template #cell-card="{ row }">
        <div class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 py-2">
          <div class="flex min-w-0 flex-1 basis-56 items-baseline gap-2">
            <RouterLink :to="linkTo(row)" class="lg-link lg-mono flex-none text-xs font-medium">{{ ticketRef(row) }}</RouterLink>
            <span class="lg-ell font-medium" v-tooltip.truncated="row.subject">{{ row.subject }}</span>
          </div>
          <div class="flex flex-none items-center gap-1.5">
            <PriorityBadge :priority="row.priority" />
            <StatusBadge :status="row.status" :title="holdTip(row)" />
          </div>
          <div class="h-0 basis-full" aria-hidden="true"></div>
          <div v-if="row.hold" class="basis-full text-xs text-muted">On hold · {{ holdTip(row) }}</div>
          <div class="lg-ell min-w-0 flex-1 basis-40 text-xs text-muted">
            {{ requesterName(row) }}<template v-if="row.organisation"> ({{ row.organisation.name }})</template> · {{ row.group.name
            }}<template v-if="!assigneeRedundant"> · {{ row.assignee ? row.assignee.name : 'Unassigned' }}</template>
          </div>
          <div class="flex flex-none items-center gap-2 text-xs text-muted">
            <SlaBadge v-if="row.sla" :sla="row.sla" />
            <span><span class="sr-only">Updated </span><RelativeTime :iso="row.activityAt" mono /></span>
          </div>
        </div>
      </template>
    </DataTable>
  </div>
</template>
