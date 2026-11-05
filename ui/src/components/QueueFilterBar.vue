<script setup lang="ts">
// Filter bar of every queue. Each chip maps to one list query parameter; the server intersects every
// filter with the view and the caller's own ticket access. A queue hides the filters it fixes itself.
// Group and assignee options are searched on the server, never loaded as a whole.
import { computed } from 'vue'
import { adminApi, organisationsApi, tagsApi, ticketsApi } from '@/api/endpoints'
import type { Member, Priority } from '@/api/types'
import { BaseButton, FilterChip, PriorityBadge, StatusBadge, type SelectOption } from '@/components/base'
import { useQuery } from '@/composables/useQuery'
import { categoryLabel, PRIORITIES, priorityShort } from '@/format'
import { emptyFilters, hasFilters, type FilterKey, type QueueFilters } from '@/filters'
import { ApiError } from '@/api/errors'
import { loadOrganisations } from '@/organisations'
import { useSessionStore } from '@/stores/session'

const props = defineProps<{ filters: QueueFilters; groupIds: string[]; fixed?: FilterKey[] }>()
const shows = (k: FilterKey) => !props.fixed?.includes(k)
const emit = defineEmits<{ change: [filters: QueueFilters, groupId: string] }>()
const session = useSessionStore()

const statusesQ = useQuery(
  () => `statuses`,
  (signal) => ticketsApi.statuses({ signal }),
)
const typesQ = useQuery(
  () => `request-types`,
  (signal) => ticketsApi.requestTypes({ signal }),
)
const tagsQ = useQuery(
  () => `tags`,
  (signal) => tagsApi.list({ signal }),
)
// Group: every page of the group list is read when the chip opens (groups are few), filtered by the search text.
const groupOne = useQuery(
  () => (props.groupIds[0] ? `group:${props.groupIds[0]}` : null),
  (signal) => adminApi.group(props.groupIds[0]!, { signal }),
)
async function loadGroups(q: string, signal: AbortSignal): Promise<SelectOption<string>[]> {
  const needle = q.toLowerCase()
  return (await adminApi.allGroups({ signal }))
    .filter((g) => !needle || g.name.toLowerCase().includes(needle))
    .map((g) => ({ value: g.id, label: g.active ? g.name : `${g.name} (inactive)` }))
}
// Assignee: searched on the server; the chosen person (when not Me/Unassigned) is read by ID for its name.
const pickedAssignee = computed(() => (props.filters.assignee && props.filters.assignee !== 'me' && props.filters.assignee !== 'unassigned' ? props.filters.assignee : ''))
const assigneeOne = useQuery(
  () => (pickedAssignee.value ? `member:${pickedAssignee.value}` : null),
  (signal) => adminApi.members({ userIds: [pickedAssignee.value], limit: 1 }, { signal }),
)

function set(patch: Partial<QueueFilters>, groupId = props.groupIds[0] ?? '') {
  emit('change', { ...props.filters, ...patch }, groupId)
}

const priority = computed({
  get: () => props.filters.priorities.map(String),
  set: (v: string[]) => set({ priorities: v.map(Number) }),
})
const priorityOptions: SelectOption<string>[] = PRIORITIES.map((p) => ({ value: String(p), label: priorityShort(p) }))
const asPriority = (v: string) => Number(v) as Priority

const status = computed({ get: () => props.filters.statusIds, set: (v: string[]) => set({ statusIds: v }) })
const statusOptions = computed(() => (statusesQ.data.value?.items ?? []).map((s) => ({ value: s.id, label: s.name, keywords: categoryLabel(s.category) })))
const statusById = computed(() => new Map((statusesQ.data.value?.items ?? []).map((s) => [s.id, s])))


const group = computed({ get: () => props.groupIds, set: (v: string[]) => set({}, v[0] ?? '') })
const groupOptions = computed(() => (groupOne.data.value ? [{ value: groupOne.data.value.id, label: groupOne.data.value.name }] : []))

const assignee = computed({
  get: () => (props.filters.assignee ? [props.filters.assignee] : []),
  set: (v: string[]) => set({ assignee: v[0] ?? '' }),
})
const personOption = (m: Member): SelectOption<string> => ({ value: m.user.id, label: m.user.name || m.user.email, hint: m.user.username, keywords: m.user.email, avatar: 'staff' })
const assigneeOptions = computed<SelectOption<string>[]>(() => {
  const picked = assigneeOne.data.value?.items[0]
  return [{ value: 'me', label: 'Me' }, { value: 'unassigned', label: 'Unassigned' }, ...(picked ? [personOption(picked)] : [])]
})
async function loadAssignees(q: string, signal: AbortSignal): Promise<SelectOption<string>[]> {
  const needle = q.toLowerCase()
  const out: SelectOption<string>[] = [{ value: 'me', label: 'Me' }, { value: 'unassigned', label: 'Unassigned' }].filter((o) => !needle || o.label.toLowerCase().includes(needle))
  const page = await adminApi.members({ q, active: true, limit: 20 }, { signal })
  for (const m of page.items) if (m.user.id !== session.user?.id) out.push(personOption(m))
  if (page.nextCursor) out.push({ value: '__more', label: 'More people match; type to narrow the list', disabled: true })
  return out
}

const tags = computed({ get: () => props.filters.tags, set: (v: string[]) => set({ tags: v }) })
const tagOptions = computed(() => (tagsQ.data.value?.items ?? []).map((t) => ({ value: t.tag, label: t.tag, hint: `${t.count}` })))
// Tags not in the tag list can still be filtered on ("Add …" in the chip's search).
function createTag(q: string): SelectOption<string> | null {
  const t = q.trim().toLowerCase().replace(/\s+/g, '-').slice(0, 40)
  return t ? { value: t, label: t } : null
}

// Organisation: searched on the server; the chosen one is read by ID for its name.
const organisation = computed({ get: () => props.filters.organisationIds.slice(0, 1), set: (v: string[]) => set({ organisationIds: v.slice(0, 1) }) })
// Filtering also offers inactive organisations (their tickets still exist); linking pickers do not.
const loadAnyOrganisation = (q: string, signal: AbortSignal) => loadOrganisations(q, signal, false, 'all')
// The `organisations:` key prefix is the one Settings → Organisations invalidates, so a rename shows here.
const organisationOne = useQuery(
  () => (props.filters.organisationIds[0] ? `organisations:one:${props.filters.organisationIds[0]}` : null),
  (signal) => organisationsApi.get(props.filters.organisationIds[0]!, { signal }),
)
const organisationOptions = computed(() => {
  const id = props.filters.organisationIds[0]
  if (!id) return []
  const o = organisationOne.data.value
  if (o) return [{ value: o.id, label: o.name, hint: o.externalRef ?? undefined }]
  // Gone (404) or not readable: say so instead of showing the raw id.
  const err = organisationOne.error.value
  if (!err) return []
  const gone = err instanceof ApiError && (err.status === 404 || err.is('organisationNotFound'))
  return [{ value: id, label: gone ? 'Unknown organisation' : 'Organisation' }]
})

const types = computed({ get: () => props.filters.requestTypeIds, set: (v: string[]) => set({ requestTypeIds: v }) })
const typeOptions = computed(() => (typesQ.data.value?.items ?? []).map((t) => ({ value: t.id, label: t.name })))

const due = computed({
  get: () => (props.filters.due ? [props.filters.due] : []),
  set: (v: string[]) => set({ due: (v[0] as QueueFilters['due']) ?? '' }),
})
const dueOptions = [
  { value: 'dueSoon', label: 'Due soon', hint: 'warning time passed' },
  { value: 'breached', label: 'Breached', hint: 'past the deadline, no reply' },
  { value: 'met', label: 'Met', hint: 'first reply in time' },
  { value: 'missed', label: 'Missed', hint: 'first reply after the deadline' },
  { value: 'running', label: 'Running', hint: 'within time' },
]

const any = computed(() => hasFilters(props.filters) || props.groupIds.length > 0)
</script>

<template>
  <div role="toolbar" aria-label="Filters" class="flex flex-wrap items-center gap-1.5">
    <FilterChip v-if="shows('status')" v-model="status" label="Status" :options="statusOptions">
      <template #option="{ option }">
        <span class="flex-1"><StatusBadge v-if="statusById.get(option.value)" :status="statusById.get(option.value)!" /></span>
      </template>
    </FilterChip>
    <FilterChip v-model="priority" label="Priority" :options="priorityOptions" any-label="All">
      <template #option="{ option }">
        <span class="flex-1"><PriorityBadge :priority="asPriority(option.value)" /></span>
      </template>
    </FilterChip>
    <FilterChip v-model="group" label="Group" :options="groupOptions" :loader="loadGroups" :multiple="false" any-label="Any" />
    <FilterChip v-if="shows('assignee')" v-model="assignee" label="Assignee" :options="assigneeOptions" :loader="loadAssignees" :multiple="false" any-label="Anyone" />
    <FilterChip v-model="tags" label="Tags" :options="tagOptions" any-label="Any" searchable :create="createTag">
      <p class="m-0 px-2 py-1 text-xs text-muted">Tickets must carry all chosen tags. Type a tag that is not listed to add it.</p>
    </FilterChip>
    <FilterChip v-model="types" label="Type" :options="typeOptions" searchable />
    <FilterChip v-model="organisation" label="Organisation" :options="organisationOptions" :loader="loadAnyOrganisation" :multiple="false" any-label="Any" />
    <FilterChip v-if="shows('sla')" v-model="due" label="SLA" :options="dueOptions" :multiple="false" />
    <BaseButton v-if="any" variant="ghost" size="sm" @click="emit('change', emptyFilters(), '')">Clear filters</BaseButton>
  </div>
</template>
