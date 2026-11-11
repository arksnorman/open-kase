<script setup lang="ts">
import { IconChevronDown } from '@tabler/icons-vue'
// Ticket activity timeline (GET .../activity?order=desc, staff only), newest first (owner decision
// 2026-10-31): who did what, which fields changed (names only, never values or bodies) and the
// recorded reason. The newest 50 events load first; "Load older activity" continues.
import { computed, useId, watch } from 'vue'
import { workflowApi } from '@/api/endpoints'
import type { ActivityItem, Ticket } from '@/api/types'
import type { AvatarRole } from '@/components/base/BaseAvatar.vue'
import { BaseAlert, BaseAvatar, BaseBadge, BaseButton, LoadingState, RelativeTime } from '@/components/base'
import { usePagedQuery } from '@/composables/usePagedQuery'

const props = defineProps<{ ticket: Ticket; queryBase: string }>()
const uid = useId()

const q = usePagedQuery<ActivityItem>(
  () => `${props.queryBase}:activity`,
  (cursor, signal) => workflowApi.activity(props.ticket.id, cursor, { signal, order: 'desc' }),
)
// Any change of the ticket adds events; reload the timeline.
watch(
  () => props.ticket.version,
  (v, old) => {
    if (old !== undefined && v !== old) void q.refetch()
  },
)
const items = computed(() => {
  const seen = new Set<string>()
  return q.items.value.filter((i) => (seen.has(i.id) ? false : (seen.add(i.id), true)))
})

const FIELD: Record<string, string> = {
  subject: 'subject',
  requester: 'requester',
  group: 'group',
  groupId: 'group',
  priority: 'priority',
  requestType: 'request type',
  requestTypeId: 'request type',
  status: 'status',
  statusId: 'status',
  assignee: 'assignee',
  assigneeId: 'assignee',
  tags: 'tags',
  watchers: 'watchers',
  body: 'message text',
}
function fieldLabel(c: string): string {
  if (FIELD[c]) return FIELD[c]
  return c.replace(/[._:]/g, ' ')
}
const LINK: Record<string, string> = { relatedTo: 'related', duplicateOf: 'duplicate', followUpTo: 'follow-up', copiedFrom: 'copy' }
const audienceOf = (i: ActivityItem) => i.changes.find((c) => c.startsWith('visibility:'))?.slice('visibility:'.length)
const linkOf = (i: ActivityItem) => i.changes.find((c) => c.startsWith('links:'))?.slice('links:'.length)
/** Files a redaction removed (`attachmentsRemoved:<n>`), 0 when none. */
const filesRemovedOf = (i: ActivityItem) =>
  Number(i.changes.find((c) => c.startsWith('attachmentsRemoved:'))?.slice('attachmentsRemoved:'.length) ?? 0)
const files = (n: number) => (n === 1 ? '1 attachment' : `${n} attachments`)

/** The action as a plain-language phrase following the actor's name. */
function phrase(i: ActivityItem): string {
  const audience = audienceOf(i) === 'agent' ? 'an agent note' : 'a public reply'
  const link = linkOf(i)
  const linkText = link ? ` (${LINK[link] ?? link.replace(/_/g, ' ')})` : ''
  switch (i.action) {
    case 'ticket.created':
      return 'created the ticket'
    case 'message.created':
      return `added ${audience}`
    case 'message.redacted': {
      const n = filesRemovedOf(i)
      if (!n) return `redacted ${audience}`
      // Without "body": the text was redacted earlier and only its files are removed now.
      return i.changes.includes('body') ? `redacted ${audience} and removed its ${files(n)}` : `removed ${files(n)} from ${audience}`
    }
    case 'ticket.claimed':
      return 'assigned the ticket to themselves'
    case 'ticket.released':
      return 'released the ticket to the group queue'
    case 'ticket.assigned':
      return 'changed the assignee'
    case 'ticket.transferred':
      return 'transferred the ticket to another group'
    case 'ticket.statusChanged':
      return 'changed the status'
    case 'ticket.resolved':
      return i.changes.some((c) => c.startsWith('contact:')) ? 'resolved the ticket on behalf of a contact' : 'resolved the ticket'
    case 'ticket.reopened':
      return 'reopened the ticket'
    case 'ticket.archived':
      return 'archived the ticket'
    case 'ticket.held':
      return 'put the ticket on hold'
    case 'ticket.holdEnded':
      return 'took the ticket off hold'
    case 'ticket.updated':
      return 'edited the ticket'
    case 'ticket.watcherAdded':
      return 'started watching'
    case 'ticket.watcherRemoved':
      return 'stopped watching'
    case 'ticket.copied':
      return 'created this ticket as a copy of another ticket'
    case 'ticket.linkAdded':
      return `linked a ticket${linkText}`
    case 'ticket.linkRemoved':
      return `removed a ticket link${linkText}`
    case 'ticket.followupReminderSent':
      return 'sent a follow-up reminder'
    case 'ticket.slaWarning':
      return 'reported that an SLA target is due soon'
    case 'ticket.slaBreached':
      return 'reported an SLA breach'
  }
  const verb = i.action.split('.').pop() ?? i.action
  return verb.replace(/_/g, ' ')
}

/** Field changes worth listing (message/link events already say what they are about). */
function changeList(i: ActivityItem): string {
  if (i.action === 'ticket.created' || i.action.startsWith('message.') || i.action.startsWith('ticket.link_')) return ''
  if (i.action === 'ticket.followupReminderSent') return ''
  const labels = [...new Set(i.changes.filter((c) => !c.startsWith('visibility:') && !c.startsWith('links:') && !c.startsWith('contact:')).map(fieldLabel))]
  return labels.join(', ')
}

function actorName(i: ActivityItem): string {
  const n = i.actor.name.trim()
  if (i.actor.type === 'system') return n ? n.charAt(0).toUpperCase() + n.slice(1) : 'System'
  return n || (i.actor.type === 'service' ? 'An API token' : i.actor.type === 'operator' ? 'An operator' : 'Someone')
}
const actorRole = (i: ActivityItem): AvatarRole => (i.actor.type === 'service' ? 'bot' : i.actor.type === 'user' ? 'staff' : 'system')
/** API tokens show the scopes that allowed the action, e.g. "API token · tickets:internal". */
const actorKind = (i: ActivityItem) =>
  i.actor.type === 'system'
    ? 'automatic'
    : i.actor.type === 'service'
      ? ['API token', ...(i.actor.scopes ?? [])].join(' · ')
      : i.actor.type === 'operator'
        ? 'operator'
        : ''
</script>

<template>
  <section class="flex flex-col gap-2" :aria-labelledby="`${uid}-h`">
    <h3 :id="`${uid}-h`" class="lg-h3">Activity</h3>
    <LoadingState v-if="q.loading.value && !q.ready.value" text="Loading activity…" size="sm" />
    <BaseAlert v-else-if="q.error.value && !q.ready.value" :error="q.error.value" />
    <p v-else-if="!items.length" class="m-0 text-muted">No recorded activity.</p>
    <template v-else>
      <ol class="m-0 flex list-none flex-col p-0" aria-label="Ticket activity, newest first">
        <li v-for="i in items" :key="i.id" class="flex flex-col gap-0.5 border-t border-line py-1.5 first:border-t-0 first:pt-0">
          <div class="flex items-baseline justify-between gap-3">
            <span class="flex min-w-0 items-baseline gap-1.5">
              <BaseAvatar :name="actorName(i)" :role="actorRole(i)" size="xs" decorative class="self-center" />
              <span class="min-w-0"><span class="font-medium">{{ actorName(i) }}</span>
              {{ ' ' }}<span>{{ phrase(i) }}</span></span>
            </span>
            <span class="flex-none text-muted"><RelativeTime :iso="i.createdAt" mono /></span>
          </div>
          <div v-if="actorKind(i) || i.outcome === 'failure'" class="flex flex-wrap gap-1.5">
            <BaseBadge v-if="actorKind(i)">{{ actorKind(i) }}</BaseBadge>
            <BaseBadge v-if="i.outcome === 'failure'" tone="danger">failed</BaseBadge>
          </div>
          <p v-if="changeList(i)" class="m-0 text-xs text-muted">Changed: {{ changeList(i) }}</p>
          <p v-if="i.reason" class="m-0 text-xs break-words"><span class="text-muted">Reason:</span> {{ i.reason }}</p>
        </li>
      </ol>
      <BaseAlert v-if="q.moreError.value" :error="q.moreError.value" />
      <BaseButton v-if="q.hasMore.value" size="sm" class="self-start" :loading="q.loadingMore.value" @click="q.loadMore"><template #icon><IconChevronDown :size="14" stroke="1.75" aria-hidden="true" /></template>Load older activity</BaseButton>
    </template>
  </section>
</template>
