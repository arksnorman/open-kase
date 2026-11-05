<script setup lang="ts">
// Audit log (GET /audit-events, admins): who did what, newest first. Events record changed field
// names and identifiers only, never message bodies or secrets.
// The action and actor filters are searchable selects over the server's standard catalogs
// (GET /audit-actions, GET /audit-actors); action labels come from the API (actionLabel).
import { IconHistory, IconRefresh } from '@tabler/icons-vue'
import { computed, onScopeDispose, ref, shallowRef, watch } from 'vue'
import { auditApi } from '@/api/endpoints'
import { ApiError, isAbortError } from '@/api/errors'
import type { AuditActorType, AuditEvent } from '@/api/types'
import type { AvatarRole } from '@/components/base/BaseAvatar.vue'
import { BaseAvatar, BaseButton, BaseModal, DataTable, FormField, RelativeTime, SelectInput, TextInput, type Column, type SelectOption } from '@/components/base'
import { ticketRef, ticketRoute } from '@/format'

// Actor value: "<type>:<id>"; "<type>:" alone means any actor of that type.
const actor = ref('')
const action = ref('')
const ticket = ref('')
const from = ref('')
const to = ref('')
// The ticket text filter applies on Enter or blur, not on every keystroke.
const applied = ref({ ticket: '' })

const actorFilter = computed(() => {
  const i = actor.value.indexOf(':')
  if (i < 0) return { actorType: '' as const, actorId: '' }
  return { actorType: actor.value.slice(0, i) as AuditActorType, actorId: actor.value.slice(i + 1) }
})

// Action catalog: static, loaded once, grouped by category.
const actionOptions = shallowRef<SelectOption<string>[]>([])
const actionsLoading = ref(true)
const actionsError = ref(false)
void auditApi
  .actions()
  .then((r) => {
    actionOptions.value = r.items.map((a) => ({
      value: a.code,
      label: a.label,
      hint: a.code,
      group: a.category,
    }))
  })
  .catch(() => (actionsError.value = true))
  .finally(() => (actionsLoading.value = false))

// Actor catalog: searched on the server (users, API tokens, system actors).
const ACTOR_AVATAR: Record<AuditActorType, AvatarRole> = { user: 'staff', token: 'bot', system: 'system' }
const ACTOR_GROUPS: Record<AuditActorType, string> = { user: 'Users', token: 'API tokens', system: 'System' }
const anyOfType: SelectOption<string>[] = [
  { value: 'user:', label: 'Any user', group: 'Any actor of a kind' },
  { value: 'token:', label: 'Any API token', group: 'Any actor of a kind' },
  { value: 'system:', label: 'Any system actor', group: 'Any actor of a kind' },
]
async function loadActors(q: string, signal: AbortSignal): Promise<SelectOption<string>[]> {
  const r = await auditApi.actors({ q, limit: 50 }, { signal })
  const needle = q.toLowerCase()
  const out = anyOfType.filter((o) => !needle || o.label.toLowerCase().includes(needle))
  for (const a of r.items) out.push({ value: `${a.type}:${a.id}`, label: a.name, hint: a.detail, group: ACTOR_GROUPS[a.type], avatar: ACTOR_AVATAR[a.type] })
  if (r.truncated) out.push({ value: '__more', label: 'More users or tokens match; type to narrow the list', disabled: true })
  return out
}

const columns: Column[] = [
  { key: 'createdAt', label: 'Time', width: '120px' },
  { key: 'actor', label: 'Actor', width: '200px' },
  { key: 'action', label: 'Action' },
  { key: 'target', label: 'Target' },
  { key: 'details', label: 'Details', width: '96px' },
]

const rows = shallowRef<AuditEvent[]>([])
const cursor = ref<string | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
let ctrl: AbortController | null = null

async function load(append: boolean): Promise<void> {
  ctrl?.abort()
  const own = new AbortController()
  ctrl = own
  loading.value = true
  error.value = null
  try {
    const page = await auditApi.list(
      {
        ...actorFilter.value,
        action: action.value,
        ticket: applied.value.ticket.trim(),
        from: from.value,
        to: to.value,
        cursor: append ? cursor.value : null,
        limit: 50,
      },
      { signal: own.signal },
    )
    if (own.signal.aborted) return
    rows.value = append ? [...rows.value, ...page.items] : page.items
    cursor.value = page.nextCursor
  } catch (err) {
    if (!isAbortError(err) && !own.signal.aborted) error.value = err
  } finally {
    if (ctrl === own) loading.value = false
  }
}

watch([actor, action, applied, from, to], () => void load(false), { immediate: true, deep: true })
onScopeDispose(() => ctrl?.abort())

function applyText() {
  if (applied.value.ticket !== ticket.value) applied.value = { ticket: ticket.value }
}

const fieldError = (f: string) => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const actorError = computed(() => fieldError('actorType') ?? fieldError('actorId'))
const actionError = computed(() => fieldError('action') ?? (actionsError.value ? 'Could not load the action list.' : undefined))
const hasFieldError = computed(() => ['action', 'ticket', 'from', 'to', 'actorType', 'actorId'].some((f) => !!fieldError(f)))
const listError = computed(() => (hasFieldError.value ? null : error.value))
const filtered = computed(() => !!(actor.value || action.value || applied.value.ticket || from.value || to.value))
function clearFilters() {
  actor.value = ''
  action.value = ''
  ticket.value = ''
  from.value = ''
  to.value = ''
  applied.value = { ticket: '' }
}

function actorLabel(e: AuditEvent): string {
  if (e.actor.type === 'token') return `${e.actor.name} (API token)`
  return e.actor.name
}
function targetLabel(e: AuditEvent): string {
  const t = e.target
  if (!t) return '—'
  if (t.ticket) return `${ticketRef(t.ticket)} · ${t.ticket.subject}`
  if (t.name) return `${t.type}: ${t.name}`
  return t.id ? `${t.type} ${t.id}` : t.type
}

const detail = shallowRef<AuditEvent | null>(null)
const detailOpen = computed({
  get: () => detail.value !== null,
  set: (v: boolean) => {
    if (!v) detail.value = null
  },
})
</script>

<template>
  <div class="flex flex-col gap-3">
    <!-- items-start: labels share one line and controls the next; a hint or error under one control
         never pushes another field's label down. Wraps on narrow screens. -->
    <form class="flex flex-wrap items-start gap-x-3 gap-y-2" @submit.prevent="applyText">
      <div class="w-full sm:w-60">
        <FormField label="Actor" :error="actorError">
          <SelectInput
            v-model="actor"
            :loader="loadActors"
            size="sm"
            placeholder="Everyone"
            search-placeholder="Name, username or email"
            empty-text="No matching actors"
            panel-class="min-w-80"
            clearable
          />
        </FormField>
      </div>
      <div class="w-full sm:w-64">
        <FormField label="Action" :error="actionError">
          <SelectInput
            v-model="action"
            :options="actionOptions"
            :loading="actionsLoading"
            searchable
            size="sm"
            placeholder="Any action"
            search-placeholder="Action or code"
            empty-text="No matching actions"
            panel-class="min-w-96"
            clearable
          />
        </FormField>
      </div>
      <div class="w-[calc(50%-6px)] sm:w-36">
        <FormField label="Ticket" hint="Number, e.g. 1042" :error="fieldError('ticket')">
          <TextInput v-model="ticket" size="sm" mono autocomplete="off" @blur="applyText" />
        </FormField>
      </div>
      <div class="w-[calc(50%-6px)] sm:w-40">
        <FormField label="From" :error="fieldError('from')"><TextInput v-model="from" type="date" size="sm" /></FormField>
      </div>
      <div class="w-[calc(50%-6px)] sm:w-40">
        <FormField label="To" :error="fieldError('to')"><TextInput v-model="to" type="date" size="sm" /></FormField>
      </div>
      <!-- Enter in a text field submits the form (applies the text filters). -->
      <button type="submit" class="sr-only">Apply filters</button>
      <div class="hidden flex-1 sm:block"></div>
      <!-- An invisible label keeps the button on the controls' line. -->
      <div class="flex flex-col gap-1.5">
        <span class="lg-label invisible" aria-hidden="true">Refresh</span>
        <div class="flex h-[34px] items-center">
          <BaseButton size="sm" :loading="loading" @click="load(false)">
            <template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>
            Refresh
          </BaseButton>
        </div>
      </div>
    </form>

    <DataTable
      :columns="columns"
      :rows="rows"
      :row-key="(e: AuditEvent) => e.id"
      label="Audit log"
      :loading="loading"
      :error="listError"
      :filtered="filtered"
      filtered-text="No audit events match these filters."
      empty-text="No audit events yet."
      :empty-icon="IconHistory"
      :has-more="!!cursor"
      :loading-more="loading && rows.length > 0"
      @retry="load(false)"
      @clear-filters="clearFilters"
      @load-more="load(true)"
    >
      <template #cell-createdAt="{ row }"><RelativeTime :iso="row.createdAt" /></template>
      <template #cell-actor="{ row }">
        <span class="flex min-w-0 items-center gap-2" v-tooltip.truncated="actorLabel(row)">
          <BaseAvatar :name="row.actor.name" :role="ACTOR_AVATAR[row.actor.type]" size="xs" decorative />
          <span class="lg-ell">{{ actorLabel(row) }}</span>
        </span>
      </template>
      <template #cell-action="{ row }">
        <span class="lg-ell block" v-tooltip.truncated="row.action">
          {{ row.actionLabel }}<span v-if="row.details.outcome === 'failure'" class="text-danger"> (failed)</span>
        </span>
      </template>
      <template #cell-target="{ row }">
        <RouterLink
          v-if="row.target?.ticket"
          :to="ticketRoute(row.target.ticket)"
          class="lg-ell block underline-offset-2 hover:underline"
          v-tooltip.truncated="targetLabel(row)"
        >
          {{ targetLabel(row) }}
        </RouterLink>
        <span v-else class="lg-ell block" :class="row.target ? '' : 'text-muted'" v-tooltip.truncated="targetLabel(row)">{{ targetLabel(row) }}</span>
      </template>
      <template #cell-details="{ row }">
        <BaseButton size="sm" variant="ghost" :aria-label="`Details of ${row.actionLabel}`" @click="detail = row">Details</BaseButton>
      </template>
    </DataTable>

    <BaseModal v-model:open="detailOpen" width="md" title="Audit event" description="Field names and identifiers only; message text and secrets are never recorded.">
      <dl v-if="detail" class="m-0 flex flex-col">
        <div class="lg-prop"><dt>Time</dt><dd><RelativeTime :iso="detail.createdAt" /></dd></div>
        <div class="lg-prop"><dt>Action</dt><dd>{{ detail.actionLabel }} <span class="lg-mono text-xs text-muted">{{ detail.action }}</span></dd></div>
        <div class="lg-prop"><dt>Outcome</dt><dd class="capitalize" :class="detail.details.outcome === 'failure' ? 'font-semibold text-danger' : detail.details.outcome === 'success' ? 'text-public' : ''">{{ detail.details.outcome }}</dd></div>
        <div class="lg-prop">
          <dt>Actor</dt>
          <dd class="flex items-center gap-2">
            <BaseAvatar :name="detail.actor.name" :role="ACTOR_AVATAR[detail.actor.type]" size="xs" decorative />
            <span>{{ actorLabel(detail) }}<span v-if="detail.actor.id" class="lg-mono text-xs text-muted"> · {{ detail.actor.id }}</span></span>
          </dd>
        </div>
        <div v-if="detail.details.label" class="lg-prop"><dt>Actor detail</dt><dd class="text-xs">{{ detail.details.label }}</dd></div>
        <div v-if="detail.actor.scopes.length" class="lg-prop"><dt>Token scopes</dt><dd class="lg-mono text-xs">{{ detail.actor.scopes.join(', ') }}</dd></div>
        <div v-if="detail.target" class="lg-prop">
          <dt>Target</dt>
          <dd class="break-all">{{ targetLabel(detail) }}<span v-if="detail.target.id" class="lg-mono text-xs text-muted"> · {{ detail.target.type }} {{ detail.target.id }}</span></dd>
        </div>
        <div v-if="detail.details.changes.length" class="lg-prop"><dt>Changed</dt><dd class="lg-mono text-xs break-all">{{ detail.details.changes.join(', ') }}</dd></div>
        <div v-if="detail.details.reason" class="lg-prop"><dt>Reason</dt><dd class="break-words">{{ detail.details.reason }}</dd></div>
        <div v-if="detail.requestId" class="lg-prop"><dt>Request ID</dt><dd class="lg-mono text-xs break-all">{{ detail.requestId }}</dd></div>
      </dl>
      <template #footer>
        <BaseButton variant="primary" @click="detail = null">Close</BaseButton>
      </template>
    </BaseModal>
  </div>
</template>
