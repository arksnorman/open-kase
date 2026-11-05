<script setup lang="ts">
// Details tab: the single place to view and edit ticket metadata. Values read as text; the
// pencil (or the value) opens one inline editor at a time. Status, priority, request type and
// assignment are handed to the ticket view, which runs them with the displayed version and
// rejects with the problem text (shown beside the field; conflicts refresh the ticket instead of
// overwriting). Assignee is where agents take a ticket ("Assign to me" claims it) or release
// their own ("Unassigned" asks for a reason). Group opens the Transfer dialog. The requester is read-only; tags save
// themselves.
import { IconArchive, IconRotateClockwise } from '@tabler/icons-vue'
import { computed, ref, useId } from 'vue'
import { errorMessage } from '@/api/errors'
import type { Priority, Ticket, WorkflowStatus } from '@/api/types'
import {
  BaseAvatar,
  InlineEditField,
  LinkButton,
  PriorityBadge,
  PrioritySelect,
  RelativeTime,
  SelectInput,
  SlaBadge,
  BackIn,
  StatusBadge,
  StatusSelect,
  type SelectOption,
} from '@/components/base'
import { priorityLong } from '@/format'
import { loadOrganisations, NO_ORGANISATION, organisationOption } from '@/organisations'
import TagsEditor from './TagsEditor.vue'
import TicketLinks from './TicketLinks.vue'
import { ASSIGN_SELF } from './assignee'

const props = defineProps<{
  ticket: Ticket
  queryBase: string
  statuses: WorkflowStatus[]
  requestTypes: SelectOption<string>[]
  assignOptions: SelectOption<string>[]
  assignValue: string
  /** Admins: searches the members of the ticket's group (options also carry the fixed entries). */
  assignLoader?: (query: string, signal: AbortSignal) => Promise<SelectOption<string>[]>
  canAssign: boolean
  /** Admins: always; agents: to assign an unassigned ticket to themselves or release their own. */
  assigneeEditable: boolean
  /** "Assign to me" is available (shown pinned in the list and as a one-click link). */
  canAssignSelf: boolean
  busy: boolean
  /** Archived: everything is read-only. */
  readonly: boolean
  /** Inline edits: each returns a promise that rejects with the problem to show. */
  onStatus?: (statusId: string) => Promise<unknown>
  onPriority?: (priority: Priority) => Promise<unknown>
  onRequestType?: (id: string) => Promise<unknown>
  /** Links the ticket to an organisation; an empty string clears it. */
  onOrganisation?: (id: string) => Promise<unknown>
  onAssign?: (value: string) => Promise<unknown>
}>()
const emit = defineEmits<{
  reopen: []
  hold: []
  archive: []
  transfer: []
  updated: [ticket: Ticket]
  stale: []
  'links-changed': []
}>()
const uid = useId()

const WORKABLE = ['open', 'waitingCustomer', 'onHold']
const workable = computed(() => WORKABLE.includes(props.ticket.status.category))
/** Inline status choices: workable statuses and the built-in Resolved (runs Resolve). On hold opens the hold dialog. */
const statusChoices = computed(() =>
  props.statuses.filter((s) => WORKABLE.includes(s.category) || (s.builtin && s.category === 'resolved')),
)
const describe = (s: { category: string }) =>
  s.category === 'resolved' ? 'resolves the ticket' : s.category === 'onHold' ? 'asks for a reason' : undefined

const saveStatus = async (id: string | null | undefined) => {
  if (!id || id === props.ticket.status.id) return
  // On hold needs a reason (and fixes the length): a small dialog instead of a plain status change.
  if (props.statuses.find((s) => s.id === id)?.category === 'onHold') {
    emit('hold')
    return
  }
  await props.onStatus?.(id)
}
const savePriority = async (p: Priority | '' | null | undefined) => {
  if (p && p !== props.ticket.priority) await props.onPriority?.(p)
}
const saveRequestType = async (id: string | null | undefined) => {
  if (id && id !== props.ticket.requestType.id) await props.onRequestType?.(id)
}
const saveOrganisation = async (id: string | null | undefined) => {
  const next = !id || id === NO_ORGANISATION ? '' : id
  if (next !== (props.ticket.organisation?.id ?? '')) await props.onOrganisation?.(next)
}
// Known options named up front, so the current value (or "No organisation") reads at once, before the search loads.
const currentOrganisation = computed<SelectOption<string>[]>(() => [
  { value: NO_ORGANISATION, label: 'No organisation' },
  ...(props.ticket.organisation ? [organisationOption(props.ticket.organisation)] : []),
])
const loadOrganisationChoices = (q: string, signal: AbortSignal) => loadOrganisations(q, signal, true)
const saveAssignee = async (value: string | null | undefined) => {
  if (value && value !== props.assignValue) await props.onAssign?.(value)
}
// One-click "Assign to me" beside an empty Assignee; problems show under the field.
const selfError = ref<string | null>(null)
const selfSaving = ref(false)
async function assignSelf() {
  selfError.value = null
  selfSaving.value = true
  try {
    await props.onAssign?.(ASSIGN_SELF)
  } catch (err) {
    selfError.value = typeof err === 'string' ? err : errorMessage(err)
  } finally {
    selfSaving.value = false
  }
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col gap-5 overflow-auto p-4">
    <section class="flex flex-col gap-2" :aria-labelledby="`${uid}-props`">
      <h3 :id="`${uid}-props`" class="lg-h3">Ticket</h3>
      <dl class="lg-props">
        <dt class="text-muted">Status</dt>
        <dd class="m-0 flex min-w-0 flex-wrap items-center gap-2">
          <InlineEditField
            label="status"
            :value="ticket.status.id"
            :editable="workable && !readonly"
            :disabled="busy"
            @save="saveStatus"
          >
            <StatusBadge :status="ticket.status" />
            <template #editor="{ editorRef, draft, commit, closed, errorId }">
              <StatusSelect
                :ref="editorRef"
                :model-value="draft"
                :statuses="statusChoices"
                :current-id="ticket.status.id"
                :describe="describe"
                size="sm"
                aria-label="Status"
                :aria-describedby="errorId"
                @update:model-value="commit"
                @close="closed"
              />
            </template>
          </InlineEditField>
          <LinkButton v-if="ticket.status.category === 'resolved' && !readonly" class="text-xs" :disabled="busy" @click="emit('reopen')">
            <IconRotateClockwise :size="14" stroke="1.75" aria-hidden="true" class="mr-0.5 inline align-[-2px]" />Reopen
          </LinkButton>
          <LinkButton v-if="ticket.status.category === 'resolved' && !readonly" class="text-xs" :disabled="busy" aria-haspopup="dialog" @click="emit('archive')">
            <IconArchive :size="14" stroke="1.75" aria-hidden="true" class="mr-0.5 inline align-[-2px]" />Archive…
          </LinkButton>
        </dd>
        <template v-if="ticket.hold">
          <dt class="text-muted">Hold reason</dt>
          <dd class="m-0 min-w-0">
            {{ ticket.hold.reasonName }}
            <span class="block text-xs text-muted">Back in <BackIn :iso="ticket.hold.until" /></span>
          </dd>
        </template>

        <dt class="text-muted">Priority</dt>
        <dd class="m-0 flex min-w-0 flex-col gap-1">
          <InlineEditField label="priority" :value="ticket.priority" :editable="!readonly" :disabled="busy" @save="savePriority">
            <PriorityBadge :priority="ticket.priority" />
            <template #editor="{ editorRef, draft, commit, closed, errorId }">
              <PrioritySelect
                :ref="editorRef"
                :model-value="draft"
                size="sm"
                aria-label="Priority"
                :aria-describedby="errorId"
                @update:model-value="commit"
                @close="closed"
              />
            </template>
          </InlineEditField>
          <span v-if="ticket.requestedPriority && ticket.requestedPriority !== ticket.priority" class="text-xs text-muted">
            Requested: {{ priorityLong(ticket.requestedPriority) }}
          </span>
        </dd>

        <dt class="text-muted">Group</dt>
        <dd class="m-0 min-w-0">
          <InlineEditField
            label="group (transfer)"
            mode="dialog"
            :display="ticket.group.name"
            :editable="canAssign && !readonly"
            :disabled="busy"
            @open="emit('transfer')"
          />
        </dd>

        <dt class="text-muted">Assignee</dt>
        <dd class="m-0 flex min-w-0 flex-col gap-1">
          <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
            <InlineEditField
              label="assignee"
              :value="assignValue"
              :display="ticket.assignee?.name ?? ''"
              empty-text="Unassigned"
              :editable="assigneeEditable && !readonly"
              :disabled="busy"
              @save="saveAssignee"
              @editing="selfError = null"
            >
              <span v-if="ticket.assignee" class="inline-flex min-w-0 items-center gap-1.5">
                <BaseAvatar :name="ticket.assignee.name" role="staff" size="xs" decorative />
                <span class="break-words">{{ ticket.assignee.name }}</span>
              </span>
              <span v-else class="text-muted">Unassigned</span>
              <template #editor="{ editorRef, draft, commit, closed, errorId }">
                <SelectInput
                  :ref="editorRef"
                  :model-value="draft"
                  :options="assignOptions"
                  :searchable="canAssign"
                  size="sm"
                  :label="canAssign ? `Members of ${ticket.group.name}` : undefined"
                  :aria-label="canAssign ? `Assignee (members of ${ticket.group.name})` : 'Assignee'"
                  :aria-describedby="errorId"
                  :search-placeholder="`Search ${ticket.group.name}`"
                  empty-text="No active members match"
                  :loader="canAssign ? assignLoader : undefined"
                  @update:model-value="commit"
                  @close="closed"
                />
              </template>
            </InlineEditField>
            <LinkButton
              v-if="!ticket.assignee && canAssignSelf && !readonly"
              tone="muted"
              class="text-xs"
              :disabled="busy || selfSaving"
              aria-keyshortcuts="T"
              v-tooltip="'Assign to me · shortcut T'"
              @click="assignSelf"
            >
              Assign to me
            </LinkButton>
          </div>
          <p v-if="selfError" role="alert" class="m-0 text-xs text-danger">{{ selfError }}</p>
        </dd>

        <dt class="text-muted">Request type</dt>
        <dd class="m-0 min-w-0">
          <InlineEditField
            label="request type"
            :value="ticket.requestType.id"
            :display="ticket.requestType.name"
            :editable="!readonly && requestTypes.length > 0"
            :disabled="busy"
            @save="saveRequestType"
          >
            <template #editor="{ editorRef, draft, commit, closed, errorId }">
              <SelectInput
                :ref="editorRef"
                :model-value="draft"
                :options="requestTypes"
                size="sm"
                aria-label="Request type"
                :aria-describedby="errorId"
                @update:model-value="commit"
                @close="closed"
              />
            </template>
          </InlineEditField>
        </dd>

        <dt class="text-muted">Source</dt>
        <dd class="m-0">{{ ticket.source === 'api' ? 'API integration' : 'Web' }}</dd>
        <template v-if="ticket.sla">
          <dt class="text-muted">SLA</dt>
          <dd class="m-0"><SlaBadge :sla="ticket.sla" variant="full" /></dd>
        </template>
        <TagsEditor :ticket="ticket" :disabled="readonly" @updated="emit('updated', $event)" @stale="emit('stale')" />
        <TicketLinks :ticket="ticket" :query-base="queryBase" @changed="emit('links-changed')" />
      </dl>
    </section>

    <section class="flex flex-col gap-2" :aria-labelledby="`${uid}-requester`">
      <h3 :id="`${uid}-requester`" class="lg-h3">Requester</h3>
      <dl class="lg-props">
        <!-- Values wrap (long addresses break anywhere) so they can always be read in full. -->
        <dt class="text-muted">Name</dt>
        <dd class="m-0 flex min-w-0 items-start gap-1.5 wrap-anywhere">
          <BaseAvatar :name="ticket.requester.name || ticket.requester.email" role="requester" size="xs" decorative class="mt-px" />
          <span class="min-w-0"><template v-if="ticket.requester.name">{{ ticket.requester.name }}</template><span v-else class="text-muted">Not available</span></span>
        </dd>
        <template v-if="ticket.requester.externalId">
          <dt class="text-muted">External ID</dt><dd class="lg-mono m-0 min-w-0 text-xs wrap-anywhere">{{ ticket.requester.externalId }}</dd>
        </template>
        <dt class="text-muted">Organisation</dt>
        <dd class="m-0 min-w-0 wrap-anywhere">
          <InlineEditField
            label="organisation"
            :value="ticket.organisation?.id ?? NO_ORGANISATION"
            :editable="!readonly && ticket.source !== 'api'"
            :disabled="busy"
            @save="saveOrganisation"
          >
            <span v-if="ticket.organisation" v-tooltip="ticket.organisation.externalRef ? `External ref: ${ticket.organisation.externalRef}` : undefined">{{ ticket.organisation.name }}</span>
            <span v-else class="text-muted">Not available</span>
            <template #editor="{ editorRef, draft, commit, closed, errorId }">
              <SelectInput
                :ref="editorRef"
                :model-value="draft"
                :options="currentOrganisation"
                :loader="loadOrganisationChoices"
                searchable
                size="sm"
                aria-label="Organisation"
                :aria-describedby="errorId"
                search-placeholder="Search organisations"
                empty-text="No organisations match"
                @update:model-value="commit"
                @close="closed"
              />
            </template>
          </InlineEditField>
        </dd>
      </dl>
    </section>



    <section class="flex flex-col gap-2" :aria-labelledby="`${uid}-dates`">
      <h3 :id="`${uid}-dates`" class="lg-h3">Dates</h3>
      <dl class="lg-props">
        <dt class="text-muted">Created</dt><dd class="m-0"><RelativeTime :iso="ticket.createdAt" /></dd>
        <dt class="text-muted">Updated</dt><dd class="m-0"><RelativeTime :iso="ticket.updatedAt" /></dd>
        <dt class="text-muted">Last activity</dt><dd class="m-0"><RelativeTime :iso="ticket.activityAt" /></dd>
        <dt class="text-muted">First reply</dt>
        <dd class="m-0"><RelativeTime v-if="ticket.firstResponseAt" :iso="ticket.firstResponseAt" /><span v-else class="text-muted">None yet</span></dd>
        <template v-if="ticket.resolvedAt">
          <dt class="text-muted">Resolved</dt><dd class="m-0"><RelativeTime :iso="ticket.resolvedAt" /></dd>
        </template>
        <template v-if="ticket.archivedAt">
          <dt class="text-muted">Archived</dt><dd class="m-0"><RelativeTime :iso="ticket.archivedAt" /></dd>
        </template>
      </dl>
    </section>
  </div>
</template>
