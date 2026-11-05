<script setup lang="ts">
import { IconArchive, IconArrowForwardUp, IconArrowLeft, IconCopy, IconLayoutSidebarRight, IconLayoutSidebarRightCollapse } from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, useId, watch } from 'vue'
import { useRoute } from 'vue-router'
import { adminApi, ticketsApi, workflowApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type {
  GroupRef,
  Message,
  PostedMessage,
  ScheduledMessage,
  Ticket,
  TicketPatch,
  TransitionInput,
} from '@/api/types'
import {
  BaseAlert,
  BaseButton,
  BaseDrawer,
  BaseModal,
  CheckboxInput,
  ErrorState,
  FormField,
  IconButton,
  LoadingState,
  PriorityBadge,
  RelativeTime,
  SelectInput,
  SlaBadge,
  StatusBadge,
  TextArea,
  type SelectOption,
} from '@/components/base'
import MessageItem from '@/components/MessageItem.vue'
import ReplyComposer from '@/components/ReplyComposer.vue'
import TicketSidePanel from '@/components/TicketSidePanel.vue'
import CopyTicketDialog from '@/components/ticket/CopyTicketDialog.vue'
import HoldDialog from '@/components/ticket/HoldDialog.vue'
import ScheduleDialog from '@/components/ticket/ScheduleDialog.vue'
import ScheduledMessageItem from '@/components/ticket/ScheduledMessageItem.vue'
import TicketDetails from '@/components/ticket/TicketDetails.vue'
import { ASSIGN_SELF, UNASSIGNED } from '@/components/ticket/assignee'
import WatchersControl from '@/components/ticket/WatchersControl.vue'
import { onLiveEvent } from '@/composables/useLiveUpdates'
import { invalidate, useQuery, queryScopeSignal } from '@/composables/useQuery'
import { useNow } from '@/composables/useNow'
import { useElementWidth } from '@/composables/useElementWidth'
import { showToast } from '@/composables/useToasts'
import { isTypingTarget, readStored, writeStored } from '@/panels'
import { holdRemaining, ticketRef } from '@/format'
import { DEFAULT_QUEUE, queueBySlug } from '@/queues'
import { useWorkspaceStore } from '@/stores/workspace'
import { useSessionStore } from '@/stores/session'

const route = useRoute()
const workspace = useWorkspaceStore()
const session = useSessionStore()

const ticketKey = computed(() => String(route.params.ticketNumber).replace(/^#/, ''))
const base = computed(() => `ticket:${ticketKey.value}`)

const ticketQ = useQuery(base, (signal) => ticketsApi.get(ticketKey.value, { signal }))
const messagesQ = useQuery(
  () => (base.value ? `${base.value}:messages` : null),
  (signal) => ticketsApi.messages(ticketKey.value, null, { signal }),
)
const statusesQ = useQuery(
  () => `statuses`,
  (signal) => ticketsApi.statuses({ signal }),
)
// Scheduled replies/notes: mine (admins: everyone's), shown after the conversation.
const scheduledQ = useQuery(
  () => (base.value ? `${base.value}:scheduled` : null),
  (signal) => ticketsApi.scheduled(ticketKey.value, { signal }),
)
const requestTypesQ = useQuery(
  () => `request-types`,
  (signal) => ticketsApi.requestTypes({ signal }),
)
const ticket = computed(() => ticketQ.data.value)

/** Admins redact and cancel others' scheduled messages. */
const isAdmin = computed(() => session.isAdmin)
/** Every agent transfers tickets to any group and assigns within the ticket's group (the API checks membership). */
const canAssign = computed(() => true)

// ---- messages (oldest first; pages beyond the first are appended locally) ----
const extraMessages = shallowRef<Message[]>([])
const messagesCursor = ref<string | null | undefined>(undefined)
const loadingMore = ref(false)
function resetPaging() {
  extraMessages.value = []
  messagesCursor.value = undefined
}
watch(base, () => {
  resetPaging()
  freshAfter.value = undefined
})
const messages = computed(() => [...(messagesQ.data.value?.items ?? []), ...extraMessages.value])
const nextMessagesCursor = computed(() =>
  messagesCursor.value === undefined ? (messagesQ.data.value?.nextCursor ?? null) : messagesCursor.value,
)
async function loadMoreMessages() {
  const cursor = nextMessagesCursor.value
  if (!cursor) return
  loadingMore.value = true
  try {
    const page = await ticketsApi.messages(ticketKey.value, cursor, { signal: queryScopeSignal() })
    extraMessages.value = [...extraMessages.value, ...page.items]
    messagesCursor.value = page.nextCursor
  } finally {
    loadingMore.value = false
  }
}

/**
 * Newest public message the agent can see. While older pages are still unloaded the newest
 * messages are not on screen, so fall back to the ticket's value only when everything is loaded.
 */
const lastPublicId = computed(() => {
  if (!messagesQ.data.value) return ticket.value?.lastPublicMessageId ?? null
  if (nextMessagesCursor.value) return ticket.value?.lastPublicMessageId ?? null
  for (let i = messages.value.length - 1; i >= 0; i--) {
    const m = messages.value[i]
    if (m && m.visibility === 'public') return m.id
  }
  return null
})

// Messages that arrived after the agent last looked (conflict or a refetch) are marked "New".
// `undefined` = nothing marked; `null` = everything after the start is new.
const freshAfter = ref<string | null | undefined>(undefined)
const freshIds = computed(() => {
  const after = freshAfter.value
  if (after === undefined) return new Set<string>()
  const list = messages.value
  const start = after === null ? 0 : list.findIndex((m) => m.id === after) + 1
  return new Set(list.slice(Math.max(0, start)).map((m) => m.id))
})
function markFreshFromNow() {
  const list = messages.value
  freshAfter.value = list.length ? list[list.length - 1]!.id : null
}

// A ticket refetch (after any mutation) can reveal a public reply that is not on screen yet.
watch(
  () => ticket.value?.lastPublicMessageId,
  async (id) => {
    if (!id || !messagesQ.data.value || nextMessagesCursor.value) return
    if (messages.value.some((m) => m.id === id)) return
    if (freshAfter.value === undefined) markFreshFromNow()
    resetPaging()
    await messagesQ.refetch()
  },
)

const archived = computed(() => ticket.value?.status.category === 'archived')
/** "On hold · <reason> · back in 3d 4h" (the status badge tooltip). */
const now = useNow()
const holdTitle = computed(() => {
  const h = ticket.value?.hold
  return h ? `On hold · ${h.reasonName} · back in ${holdRemaining(h.until, Math.max(now.value, Date.now()))}` : undefined
})
const resolved = computed(() => ticket.value?.status.category === 'resolved')
const mine = computed(() => !!ticket.value?.assignee && ticket.value.assignee.id === session.user?.id)
const backQueue = computed(() => {
  const from = typeof route.query.from === 'string' ? queueBySlug(route.query.from) : undefined
  return from ?? queueBySlug(DEFAULT_QUEUE)!
})

// ---- mutations ----
const busy = ref(false)
const actionError = ref<unknown>(null)
const actionNotice = ref<string | null>(null)

function invalidateLists() {
  invalidate('tickets:')
  invalidate('queue-counts')
}

function describeError(err: unknown): unknown {
  if (!(err instanceof ApiError)) return err
  switch (err.code) {
    case 'ticketAlreadyClaimed':
      return 'Someone else took this ticket first. The ticket has been refreshed.'
    case 'versionConflict':
      return 'The ticket changed since you loaded it (for example a new message or another agent’s change). It has been refreshed; check the current values and try again.'
    case 'ticketArchived':
      return 'Archived tickets cannot be changed. Use “Create follow-up ticket” to continue the work in a new, linked ticket.'
    case 'invalidTransition':
      return 'That status change is not allowed from the ticket’s current status. The ticket has been refreshed.'
    case 'notGroupMember':
      return `Only members of ${ticket.value?.group.name ?? 'the ticket’s group'} can assign this ticket to themselves.`
    case 'ticketNotAssigned':
      return 'This ticket is no longer assigned. The ticket has been refreshed.'
    case 'ticketNotFound':
      return 'This ticket is no longer available (it may have been moved out of your groups).'
  }
  if (err.status === 422 && err.fieldErrors.some((f) => f.field === 'assigneeId'))
    return 'That person is no longer an active member of the chosen group. The ticket and member list have been refreshed.'
  if (err.status === 422 && err.fieldErrors.some((f) => f.field === 'groupId'))
    return 'That group is no longer active. The ticket has been refreshed; choose another group.'
  return err
}

/** Runs a ticket mutation with the displayed version; returns true on success. */
async function mutate(run: (t: Ticket) => Promise<Ticket | void>, opts: { fresh?: boolean; inline?: boolean } = {}): Promise<boolean> {
  let t = ticket.value
  if (opts.inline && t && busy.value) throw 'Another change is still saving. Try again in a moment.'
  if (!t || busy.value) return false
  busy.value = true
  actionError.value = null
  actionNotice.value = null
  try {
    if (opts.fresh) {
      // Messages bump `version`; read the latest version first so a claim is decided on the
      // current state (an already-claimed ticket is reported, not overwritten).
      t = await ticketsApi.get(t.id, { signal: queryScopeSignal() })
      ticketQ.set(t)
    }
    const result = await run(t)
    if (result && typeof result === 'object' && 'version' in result) ticketQ.set(result)
    else await ticketQ.refetch()
    invalidateLists()
    return true
  } catch (err) {
    if (!opts.inline) actionError.value = describeError(err)
    if (err instanceof ApiError && (err.status === 409 || err.status === 404 || err.status === 403 || err.status === 422)) {
      await ticketQ.refetch()
    }
    // Inline (Details panel) edits show the problem beside the field instead of the banner.
    if (opts.inline) throw describeError(err)
    return false
  } finally {
    busy.value = false
  }
}

/** "Assign to me" via claim: unassigned, unresolved, live tickets (members of the ticket's group). */
const canClaim = computed(() => !!ticket.value && !ticket.value.assignee && !resolved.value && !archived.value)
const canRelease = computed(() => !!ticket.value?.assignee && !archived.value && (mine.value || isAdmin.value))

// ---- release (a reason is required: 3–500 characters, ticket.releaseReason) ----
const releaseOpen = ref(false)
const releaseReason = ref('')
const releaseError = ref<unknown>(null)
const releasing = ref(false)
function openRelease() {
  releaseReason.value = ''
  releaseError.value = null
  releaseOpen.value = true
}
async function doRelease() {
  const reason = releaseReason.value.trim()
  if (reason.length < 3) return
  releasing.value = true
  releaseError.value = null
  const ok = await mutate((t) => ticketsApi.release(t, reason))
  releasing.value = false
  if (ok) {
    releaseOpen.value = false
    if (base.value) invalidate(`${base.value}:activity`)
  } else {
    releaseError.value = actionError.value
    actionError.value = null
  }
}

// ---- keyboard: T assigns the ticket to me; ] shows or hides the Details/Activity panel ----
function onGlobalKey(e: KeyboardEvent) {
  if (isTypingTarget(e.target) || e.metaKey || e.ctrlKey || e.altKey || e.repeat) return
  if (document.querySelector('dialog[open]')) return
  if ((e.key === 't' || e.key === 'T') && canAssignSelf.value && !busy.value) {
    e.preventDefault()
    void assignToMe()
  } else if (e.key === ']' && ticket.value) {
    e.preventDefault()
    togglePanel()
  }
}
onMounted(() => document.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onGlobalKey))
const toggleWatch = () =>
  mutate((t) => (t.watching ? ticketsApi.unwatch(t) : ticketsApi.watch(t)))
const transition = (input: TransitionInput) => mutate((t) => ticketsApi.transition(t, input))

// ---- status ----
const workableStatuses = computed(() =>
  (statusesQ.data.value?.items ?? []).filter((s) => ['open', 'waitingCustomer'].includes(s.category)),
)

// ---- inline edits from the Details panel (PATCH priority / request type, status) ----
const requestTypeOptions = computed(() => (requestTypesQ.data.value?.items ?? []).map((r) => ({ value: r.id, label: r.name })))
const patchTicket = (patch: TicketPatch) => mutate((t) => ticketsApi.update(t, patch), { inline: true })
async function onDetailsStatus(id: string) {
  const st = allStatuses.value.find((x) => x.id === id)
  if (!st) return
  const input: TransitionInput = st.category === 'resolved' ? { action: 'resolve' } : { action: 'setStatus', statusId: id }
  await mutate((t) => ticketsApi.transition(t, input), { inline: true })
}

// ---- on hold (the Status picker opens the dialog: a reason is required) ----
const holdOpen = ref(false)
const holdError = ref<unknown>(null)
const holdBusy = ref(false)
function openHold() {
  holdError.value = null
  holdOpen.value = true
}
async function doHold(v: { holdReasonId: string; note: string }) {
  holdBusy.value = true
  holdError.value = null
  try {
    await mutate((t) => ticketsApi.transition(t, { action: 'hold', holdReasonId: v.holdReasonId, ...(v.note ? { note: v.note } : {}) }), { inline: true })
    holdOpen.value = false
    if (base.value) invalidate(`${base.value}:messages`)
  } catch (err) {
    holdError.value = err
  } finally {
    holdBusy.value = false
  }
}

// ---- archive / follow-up (opened from the Details panel) ----
const archiveConfirm = ref(false)
const followUpOpen = ref(false)
function confirmArchive() {
  archiveConfirm.value = false
  void transition({ action: 'archive' })
}

// ---- assignment and transfer (every agent) ----
// Assignee pickers search the member directory on the server (active members of one group, 20 per
// search) instead of holding every user in the browser.
const personLabel = (m: { user: { name: string; email: string } }) => m.user.name || m.user.email
/** Options loader for "active members of `groupId`", with the fixed entries (Unassigned, Assign to me) first. */
function memberLoader(groupId: () => string | undefined, pinned: () => SelectOption<string>[]) {
  return async (q: string, signal: AbortSignal): Promise<SelectOption<string>[]> => {
    const gid = groupId()
    if (!gid) return []
    const needle = q.toLowerCase()
    const out = pinned().filter((o) => !needle || o.label.toLowerCase().includes(needle))
    const me = session.user?.id
    const pinnedValues = new Set(out.map((o) => o.value))
    const page = await adminApi.members({ q, groupId: gid, active: true, limit: 20 }, { signal })
    for (const m of page.items) {
      if (m.user.id === me && pinnedValues.has(ASSIGN_SELF)) continue
      out.push({
        value: m.user.id,
        label: m.user.id === me ? `${personLabel(m)} (you)` : personLabel(m),
        hint: m.user.username,
        keywords: m.user.email,
        avatar: 'staff',
      })
    }
    if (page.nextCursor) out.push({ value: '__more', label: 'More members match; type to narrow the list', disabled: true })
    return out
  }
}

// ---- Details panel Assignee field: the place to take, hand over or release a ticket ----
/** The current user is a member of the ticket's group (claim is allowed). */
const inTicketGroup = computed(() => {
  const t = ticket.value
  const me = session.user?.id
  if (!t || !me) return false
  return workspace.myGroups.some((g) => g.id === t.group.id)
})
/**
 * "Assign to me": agents claim unassigned live tickets; admins can also assign themselves
 * (assignments API) when they are an active member of the ticket's group.
 */
const canAssignSelf = computed(() => {
  if (!ticket.value || archived.value || mine.value) return false
  if (canAssign.value) return inTicketGroup.value
  return canClaim.value
})
/** Plain agents edit the field only to take an unassigned ticket or release their own. */
const assigneeEditable = computed(() => !archived.value && (canAssign.value || canAssignSelf.value || mine.value))
const assignOptions = computed<SelectOption<string>[]>(() => {
  const t = ticket.value
  const user = session.user
  if (!t || !user) return []
  const self: SelectOption<string>[] = canAssignSelf.value ? [{ value: ASSIGN_SELF, label: 'Assign to me' }] : []
  if (!canAssign.value) {
    const current: SelectOption<string> = mine.value
      ? { value: user.id, label: `${user.name || user.email} (you)`, avatar: 'staff' }
      : { value: UNASSIGNED, label: 'Unassigned' }
    return mine.value ? [current, { value: UNASSIGNED, label: 'Unassigned' }] : [...self, current]
  }
  // Admins: "Assign to me" pinned first, then Unassigned; the loader adds the group's members. The
  // current assignee is listed here so the field can name them before a search runs.
  const current: SelectOption<string>[] = t.assignee
    ? [{ value: t.assignee.id, label: t.assignee.id === user.id ? `${t.assignee.name} (you)` : t.assignee.name, avatar: 'staff' }]
    : []
  return [...self, { value: UNASSIGNED, label: 'Unassigned' }, ...current]
})
/** Admins' Assignee picker: members of the ticket's group, searched on the server. */
const assignLoader = memberLoader(
  () => ticket.value?.group.id,
  () => [...(canAssignSelf.value ? [{ value: ASSIGN_SELF, label: 'Assign to me' }] : []), { value: UNASSIGNED, label: 'Unassigned' }],
)
const assignValue = computed(() => ticket.value?.assignee?.id ?? UNASSIGNED)

/** Claim when allowed (atomic; a race reports ticketAlreadyClaimed), otherwise assign. */
function assignToMe(inline = false) {
  const me = session.user?.id
  if (!me) return Promise.resolve(false)
  if (canClaim.value && (!canAssign.value || inTicketGroup.value))
    return mutate(
      (t) => {
        if (t.assignee) throw new ApiError(409, { code: 'ticketAlreadyClaimed' })
        return ticketsApi.claim(t)
      },
      { fresh: true, inline },
    )
  return mutate((t) => ticketsApi.assign(t, { groupId: t.group.id, assigneeId: me }), { inline })
}
async function doAssign(value: string) {
  const t = ticket.value
  if (!t || value === (t.assignee?.id ?? UNASSIGNED)) return
  if (value === ASSIGN_SELF) {
    await assignToMe(true)
    return
  }
  // Unassigning returns the ticket to the group queue: the release flow records a reason.
  if (value === UNASSIGNED) {
    if (!canRelease.value) throw 'You cannot release this ticket.'
    openRelease()
    return
  }
  await mutate((cur) => ticketsApi.assign(cur, { groupId: cur.group.id, assigneeId: value }), { inline: true })
}

const transferOpen = ref(false)
const transferGroups = shallowRef<GroupRef[]>([])
const transferTarget = ref<string>('')
const transferAssignee = ref<string>(UNASSIGNED)
async function openTransfer() {
  transferGroups.value = workspace.myGroups
  transferTarget.value = ''
  transferAssignee.value = UNASSIGNED
  transferOpen.value = true
  try {
    // Every active group, page by page (the list can exceed one page).
    transferGroups.value = (await adminApi.allGroups({ signal: queryScopeSignal() })).filter((g) => g.active)
  } catch {
    // keep fallback list
  }
}
const transferOptions = computed(() =>
  transferGroups.value.map((g) => ({
    value: g.id,
    label: g.id === ticket.value?.group.id ? `${g.name} (current)` : g.name,
    disabled: g.id === ticket.value?.group.id,
  })),
)
const transferLoader = memberLoader(() => transferTarget.value, () => [{ value: UNASSIGNED, label: 'Unassigned' }])
/** Whether the current assignee is an active member of the chosen group (they can follow the ticket there). */
const assigneeFollows = ref<boolean | null>(null)
// Choosing a group preselects the current assignee when they can follow the ticket there.
watch(transferTarget, async (groupId) => {
  transferAssignee.value = UNASSIGNED
  assigneeFollows.value = null
  const current = ticket.value?.assignee?.id
  if (!groupId || !current) return
  try {
    const page = await adminApi.members({ groupId, userIds: [current], active: true, limit: 1 }, { signal: queryScopeSignal() })
    if (transferTarget.value !== groupId) return
    assigneeFollows.value = page.items.length > 0
    if (assigneeFollows.value && transferAssignee.value === UNASSIGNED) transferAssignee.value = current
  } catch {
    // the ticket then transfers unassigned unless an assignee is chosen
  }
})
const transferNote = computed(() => {
  const current = ticket.value?.assignee
  if (!current || !transferTarget.value || transferAssignee.value !== UNASSIGNED || assigneeFollows.value === null) return null
  return assigneeFollows.value
    ? `${current.name} will be unassigned.`
    : `${current.name} is not an active member of this group, so the ticket will be unassigned.`
})
function doTransfer() {
  const groupId = transferTarget.value
  if (!groupId) return
  const assignee = transferAssignee.value
  transferOpen.value = false
  void mutate((t) =>
    ticketsApi.assign(t, { groupId: groupId, assigneeId: assignee === UNASSIGNED ? null : assignee }),
  )
}

// ---- conversation ----
const conversation = ref<HTMLElement | null>(null)
async function scrollToEnd() {
  await nextTick()
  conversation.value?.scrollTo({ top: conversation.value.scrollHeight })
}
async function onSent(posted: PostedMessage) {
  // The response carries the updated ticket (new version), so claims/transitions that
  // follow use the current version without another read.
  // Messages first, so the ticket's new lastPublicMessageId is already on screen and the
  // agent's own reply is not marked New.
  freshAfter.value = undefined
  resetPaging()
  await messagesQ.refetch()
  ticketQ.set(posted.ticket)
  invalidateLists()
  showToast(
    posted.visibility === 'agent'
      ? { tone: 'success', title: 'Note added', detail: `${ticketRef(posted.ticket)} · visible to support staff only` }
      : { tone: 'success', title: 'Reply sent', detail: `${ticketRef(posted.ticket)} · visible to the requester` },
  )
  await scrollToEnd()
}
async function onConflict() {
  markFreshFromNow()
  resetPaging()
  await messagesQ.refetch()
  await ticketQ.refetch()
  await nextTick()
  const first = conversation.value?.querySelector<HTMLElement>('[data-fresh="true"]')
  if (first) first.scrollIntoView({ block: 'center' })
  else await scrollToEnd()
}

/** A change made by the side panel returned the updated ticket. */
function onTicketUpdated(t: Ticket) {
  ticketQ.set(t)
  invalidateLists()
  invalidate(`tags`)
}
/** A change was refused because the ticket moved on; load the current version. */
async function onStale() {
  await ticketQ.refetch()
  invalidateLists()
}

// ---- live updates ----
// Another person's (or automation's) change to this ticket: refetch it. A new public reply
// then flows through the `lastPublicMessageId` watcher above (marked New; the composer
// shows its conflict notice instead of touching the draft).
let liveRefetching: Promise<void> | null = null
let liveAgain = false
async function liveRefresh(messagesToo: boolean) {
  if (liveRefetching) {
    liveAgain = true
    return
  }
  const run = (async () => {
    const before = ticket.value?.lastPublicMessageId
    await ticketQ.refetch()
    // Agent notes and redactions do not move lastPublicMessageId; reload the conversation.
    if (messagesToo && messagesQ.data.value && ticket.value?.lastPublicMessageId === before) {
      if (!nextMessagesCursor.value) {
        if (freshAfter.value === undefined) markFreshFromNow()
        resetPaging()
      }
      await messagesQ.refetch()
    }
    if (base.value) {
      invalidate(`${base.value}:links`)
      invalidate(`${base.value}:activity`)
    }
  })()
  liveRefetching = run
  try {
    await run
  } finally {
    liveRefetching = null
    if (liveAgain) {
      liveAgain = false
      void liveRefresh(true)
    }
  }
}
onLiveEvent((e) => {
  const t = ticket.value
  if (!t) return
  if (e.kind === 'resync') {
    void liveRefresh(true)
    return
  }
  if (e.event.ticketId !== t.id) return
  // Scheduled messages (held for review, cancelled by the system) do not move the version either.
  if (e.event.type.startsWith('scheduledMessage.')) {
    void scheduledQ.refetch()
    return
  }
  // A scheduled message was just sent by the server: refresh the pending list too.
  if (e.event.type === 'message.created' && scheduled.value.length) void scheduledQ.refetch()
  // Links do not move the ticket version: refresh the Links section and the timeline only.
  if (e.event.type.startsWith('ticket.link_')) {
    if (base.value) {
      invalidate(`${base.value}:links`)
      invalidate(`${base.value}:activity`)
    }
    return
  }
  // Already on screen (our own mutation returned this version). Integrations get version 0.
  if (e.event.version > 0 && e.event.version <= t.version) return
  void liveRefresh(e.event.type.startsWith('message.'))
})

// ---- redaction (admins) ----
const canRedact = isAdmin
const redactTarget = shallowRef<Message | null>(null)
const redactOpen = ref(false)
const redactReason = ref('')
const redactConfirm = ref(false)
/** Also remove the message's files (`removeAttachments`); off by default. */
const redactFiles = ref(false)
const redactError = ref<unknown>(null)
const redacting = ref(false)
/** Files of the target message that can still be downloaded. */
const redactKept = computed(() => redactTarget.value?.attachments.filter((a) => !a.removed).length ?? 0)
const redactFilesLabel = computed(() => (redactKept.value === 1 ? '1 attachment' : `${redactKept.value} attachments`))
/** The body was redacted earlier with its files kept: this removes only the files. */
const redactFilesOnly = computed(() => !!redactTarget.value?.redactedAt)
function openRedact(m: Message) {
  redactTarget.value = m
  redactReason.value = ''
  redactConfirm.value = false
  redactFiles.value = false
  redactError.value = null
  redactOpen.value = true
}
async function doRedact() {
  const t = ticket.value
  const m = redactTarget.value
  if (!t || !m) return
  const filesOnly = redactFilesOnly.value
  redacting.value = true
  redactError.value = null
  try {
    await workflowApi.redact(t.id, m.id, redactReason.value.trim(), filesOnly || (redactFiles.value && redactKept.value > 0))
    redactOpen.value = false
    resetPaging()
    await Promise.all([messagesQ.refetch(), ticketQ.refetch()])
    if (base.value) invalidate(`${base.value}:activity`)
    invalidateLists()
  } catch (err) {
    if (err instanceof ApiError && err.is('messageAlreadyRedacted')) {
      redactOpen.value = false
      actionNotice.value = filesOnly ? 'The attachments of that message were already removed.' : 'That message was already redacted.'
      resetPaging()
      await messagesQ.refetch()
    } else redactError.value = err instanceof ApiError && err.field('reason') ? err.field('reason') : err
  } finally {
    redacting.value = false
  }
}

// Who redacted which message: the activity timeline records it (message.redacted, target
// message/<id>); the message itself only carries redactedAt. A later removal of the files kept
// then is a message.redacted event without "body" and does not change who redacted the text.
const redactedBy = shallowRef<Map<string, string>>(new Map())
const redactedIds = computed(() => messages.value.filter((m) => m.redactedAt).map((m) => m.id))
let redactorCtrl: AbortController | null = null
watch(
  () => [redactedIds.value.join(','), ticket.value?.version] as const,
  async ([ids]) => {
    redactorCtrl?.abort()
    const t = ticket.value
    if (!ids || !t) return
    if (ids.split(',').every((id) => redactedBy.value.has(id))) return
    const ctrl = new AbortController()
    redactorCtrl = ctrl
    const found = new Map<string, string>()
    const wanted = ids.split(',')
    let cursor: string | null = null
    try {
      // Only message.redacted events, newest first; stop as soon as every redacted message is named.
      for (let i = 0; i < 5; i++) {
        const page: Awaited<ReturnType<typeof workflowApi.activity>> = await workflowApi.activity(t.id, cursor, {
          signal: ctrl.signal,
          action: 'message.redacted',
          order: 'desc',
          limit: 50,
        })
        for (const a of page.items)
          if (a.target.startsWith('message/') && a.changes.includes('body') && !found.has(a.target.slice('message/'.length)))
            found.set(a.target.slice('message/'.length), a.actor.name)
        cursor = page.nextCursor
        if (!cursor || wanted.every((id) => found.has(id))) break
      }
      if (!ctrl.signal.aborted) redactedBy.value = found
    } catch {
      // the name is optional; the message still shows as redacted
    }
  },
  { immediate: true },
)
watch(base, () => (redactedBy.value = new Map()))

// ---- scheduled messages ----
const scheduled = computed(() => scheduledQ.data.value?.items ?? [])
const allStatuses = computed(() => statusesQ.data.value?.items ?? [])
/** Statuses a scheduled message may set when sent (as in the composer's menu). */
const thenStatuses = computed(() =>
  allStatuses.value.filter((s) => workableStatuses.value.includes(s) || (s.builtin && s.category === 'resolved')),
)
const scheduledBusy = ref<string | null>(null)
const scheduledError = ref<unknown>(null)
async function onScheduled(sm: ScheduledMessage) {
  await scheduledQ.refetch()
  actionNotice.value = null
  showToast({ tone: 'success', title: sm.visibility === 'agent' ? 'Note scheduled' : 'Reply scheduled', detail: 'Shown under “Not sent yet” until it is sent.' })
  await scrollToEnd()
}
async function sendScheduledNow(sm: ScheduledMessage) {
  scheduledBusy.value = sm.id
  scheduledError.value = null
  try {
    const posted = await ticketsApi.sendScheduledNow(ticketKey.value, sm, lastPublicId.value)
    await onSent(posted)
    await scheduledQ.refetch()
  } catch (err) {
    if (err instanceof ApiError && err.is('newerReplyExists')) {
      scheduledError.value = 'A newer public reply arrived. It is shown above, marked New. Review it, then send again.'
      await onConflict()
    } else {
      scheduledError.value = describeError(err)
      await Promise.all([scheduledQ.refetch(), ticketQ.refetch()])
    }
  } finally {
    scheduledBusy.value = null
  }
}
async function cancelScheduled(sm: ScheduledMessage) {
  scheduledBusy.value = sm.id
  scheduledError.value = null
  try {
    await ticketsApi.cancelScheduled(ticketKey.value, sm)
    showToast({ tone: 'success', title: sm.visibility === 'public' ? 'Scheduled reply cancelled' : 'Scheduled note cancelled' })
  } catch (err) {
    scheduledError.value = describeError(err)
  } finally {
    scheduledBusy.value = null
    await scheduledQ.refetch()
  }
}
const editTarget = shallowRef<ScheduledMessage | null>(null)
const editScheduledOpen = ref(false)
const editScheduledError = ref<unknown>(null)
const editScheduledBusy = ref(false)
function openEditScheduled(sm: ScheduledMessage) {
  editTarget.value = sm
  editScheduledError.value = null
  editScheduledOpen.value = true
}
async function saveScheduled(value: { sendAt: string; thenStatusId: string | null; body?: string }) {
  const sm = editTarget.value
  if (!sm) return
  editScheduledBusy.value = true
  editScheduledError.value = null
  try {
    await ticketsApi.updateScheduled(ticketKey.value, sm, {
      sendAt: value.sendAt,
      thenStatusId: value.thenStatusId,
      ...(value.body !== undefined && value.body !== sm.body ? { body: value.body } : {}),
    })
    editScheduledOpen.value = false
    await scheduledQ.refetch()
  } catch (err) {
    editScheduledError.value = describeError(err)
    if (err instanceof ApiError && err.status === 409) await scheduledQ.refetch()
  } finally {
    editScheduledBusy.value = false
  }
}

/** The ticket itself could not be loaded: a page heading and the reason, then retry or go back. */
const loadFailure = computed(() => {
  const err = ticketQ.error.value
  const key = ticketKey.value
  if (err instanceof ApiError && err.status === 404)
    return { heading: `Ticket ${key} not found`, title: 'The link may be wrong, or the ticket is not in your groups.' }
  if (err instanceof ApiError && err.status === 403) return { heading: `Ticket ${key}`, title: 'You do not have access to this ticket' }
  return { heading: `Ticket ${key}`, title: 'The ticket could not be loaded' }
})

// ---- side panel: beside the thread, or over it on a narrow workspace (below 960 px) ----
const workspaceEl = ref<HTMLElement | null>(null)
const workspaceWidth = useElementWidth(workspaceEl)
const panelOverlay = computed(() => workspaceWidth.value > 0 && workspaceWidth.value < 960)
const panelOpen = ref(false)
const panelTarget = `ticket-panel-${useId()}`
const panelId = `ticket-side-${useId()}`
watch(panelOverlay, (v) => {
  if (!v) panelOpen.value = false
})
/** Beside the thread (wide workspace) the panel can be hidden; remembered per browser. */
const PANEL_KEY = 'oc.ticketPanel'
const panelHidden = ref(readStored(PANEL_KEY) === 'hidden')
watch(panelHidden, (v) => writeStored(PANEL_KEY, v ? 'hidden' : 'shown'))
/** The panel is on screen: beside the thread, or open in the drawer. */
const panelShown = computed(() => (panelOverlay.value ? panelOpen.value : !panelHidden.value))
function togglePanel() {
  if (panelOverlay.value) panelOpen.value = !panelOpen.value
  else panelHidden.value = !panelHidden.value
}
</script>

<template>
  <div ref="workspaceEl" class="flex min-h-0 flex-1">
    <div class="flex min-w-0 flex-1 flex-col bg-bg">
      <!-- One compact bar: way back, identifier, state chips; watch, watchers, actions, panel toggle. Wraps on narrow screens. -->
      <div class="flex min-h-[52px] flex-none flex-wrap items-center gap-x-3 gap-y-1.5 border-b border-border bg-surface px-5 py-2">
        <nav aria-label="Breadcrumb" class="flex min-w-0 items-center gap-1.5 text-muted">
          <RouterLink :to="{ name: 'queue', params: { view: backQueue.slug } }" class="lg-link inline-flex min-w-0 items-center gap-1">
            <IconArrowLeft :size="16" stroke="1.75" aria-hidden="true" class="flex-none" /> <span class="lg-ell">{{ backQueue.label }}</span>
          </RouterLink>
          <span aria-hidden="true">/</span>
          <span aria-current="page" class="lg-mono flex-none text-xs font-medium text-text">{{ ticket ? ticketRef(ticket) : `#${ticketKey}` }}</span>
        </nav>
        <div v-if="ticket" role="group" aria-label="Ticket state" class="flex min-w-0 flex-wrap items-center gap-1.5">
          <StatusBadge :status="ticket.status" :title="holdTitle" />
          <PriorityBadge :priority="ticket.priority" short />
          <SlaBadge v-if="ticket.sla" :sla="ticket.sla" variant="full" />
        </div>
        <span class="flex-1"></span>
        <div v-if="ticket" class="flex flex-none flex-wrap items-center justify-end gap-1.5">
          <!-- Open on archived tickets too: the only way to continue an archived conversation. -->
          <IconButton label="Copy or create a follow-up ticket" variant="bordered" aria-haspopup="dialog" @click="followUpOpen = true">
            <IconCopy :size="16" stroke="1.75" />
          </IconButton>
          <WatchersControl :ticket="ticket" :query-base="base ?? ''" :busy="busy" :archived="archived" :error="actionError" @toggle="toggleWatch" />
          <IconButton
            :label="panelShown ? 'Hide details panel (])' : 'Show details panel (])'"
            variant="bordered"
            :expanded="panelShown"
            :aria-controls="panelOverlay ? undefined : panelId"
            :aria-haspopup="panelOverlay ? 'dialog' : undefined"
            @click="togglePanel"
          >
            <IconLayoutSidebarRightCollapse v-if="panelShown" :size="16" stroke="1.75" />
            <IconLayoutSidebarRight v-else :size="16" stroke="1.75" />
          </IconButton>
        </div>
      </div>

      <div v-if="ticketQ.error.value && !ticket" class="flex flex-col items-start gap-1 p-5">
        <h1 class="m-0 text-[19px] font-semibold tracking-[-0.01em]">{{ loadFailure.heading }}</h1>
        <ErrorState
          class="self-stretch"
          :error="describeError(ticketQ.error.value)"
          :title="loadFailure.title"
          :retrying="ticketQ.loading.value"
          @retry="ticketQ.refetch"
        />
        <BaseButton :to="{ name: 'queue', params: { view: backQueue.slug } }">
          <template #icon><IconArrowLeft :size="16" stroke="1.75" aria-hidden="true" /></template>
          Back to {{ backQueue.label }}
        </BaseButton>
      </div>
      <LoadingState v-else-if="!ticket" text="Loading ticket…" class="m-5" />

      <template v-if="ticket">
        <header class="flex flex-none flex-col gap-3 border-b border-border bg-surface px-5 pt-3 pb-3">
          <h1 class="m-0 text-[19px] font-semibold tracking-[-0.01em] break-words">{{ ticket.subject }}</h1>

          <BaseAlert v-if="actionError" :error="actionError" />
          <BaseAlert v-if="actionNotice" tone="info" :error="actionNotice" />
          <BaseAlert v-if="archived" tone="warn">
            <span class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
              <span class="flex-1">This ticket is archived and read-only. Create a linked follow-up ticket for further work.</span>
              <BaseButton size="sm" variant="primary" aria-haspopup="dialog" @click="followUpOpen = true"><template #icon><IconArrowForwardUp :size="14" stroke="1.75" aria-hidden="true" /></template>Create follow-up ticket</BaseButton>
            </span>
          </BaseAlert>
        </header>

        <!-- Focusable so the conversation can be scrolled with the keyboard. -->
        <section ref="conversation" aria-label="Conversation" tabindex="0" class="flex min-h-0 flex-1 flex-col gap-3 overflow-auto px-5 py-4">
          <BaseAlert v-if="messagesQ.error.value" :error="messagesQ.error.value" />
          <LoadingState v-else-if="messagesQ.loading.value && !messagesQ.data.value" text="Loading conversation…" />
          <p v-else-if="!messages.length" class="m-0 text-muted">No messages yet.</p>
          <MessageItem
            v-for="(m, i) in messages"
            :key="m.id"
            :newest="!nextMessagesCursor && i === messages.length - 1"
            :message="m"
            :ticket="ticket"
            :fresh="freshIds.has(m.id)"
            :data-fresh="freshIds.has(m.id) ? 'true' : undefined"
            :can-redact="canRedact"
            :redacted-by="redactedBy.get(m.id) ?? null"
            @redact="openRedact"
          />
          <BaseButton v-if="nextMessagesCursor" size="sm" class="self-center" :loading="loadingMore" @click="loadMoreMessages">
            Load more messages
          </BaseButton>
          <template v-if="scheduled.length">
            <h2 class="m-0 mt-1 text-xs font-semibold tracking-wide text-muted uppercase">Not sent yet</h2>
            <BaseAlert v-if="scheduledError" :error="scheduledError" />
            <ScheduledMessageItem
              v-for="sm in scheduled"
              :key="sm.id"
              :item="sm"
              :statuses="allStatuses"
              :mine="sm.author.id === session.user?.id"
              :can-cancel="sm.author.id === session.user?.id || isAdmin"
              :busy="scheduledBusy === sm.id"
              :locked="archived"
              @edit="openEditScheduled(sm)"
              @send-now="sendScheduledNow(sm)"
              @cancel="cancelScheduled(sm)"
            />
          </template>
        </section>

        <!-- One composer per ticket: when another (cached) ticket opens in this view, the old
             composer unmounts and saves its pending text to its own ticket (OC-UIC-14). -->
        <ReplyComposer
          :key="ticket.id"
          :ticket="ticket"
          :last-public-id="lastPublicId"
          :disabled="archived"
          :statuses="allStatuses"
          @sent="onSent"
          @scheduled="onScheduled"
          @conflict="onConflict"
          @stale="onStale"
          @updated="onTicketUpdated"
        />
      </template>
    </div>

    <!-- One panel instance: beside the thread, or moved (not remounted) into the drawer below. -->
    <Teleport defer :to="`#${panelTarget}`" :disabled="!panelOverlay">
      <TicketSidePanel
        v-if="ticket"
        v-show="panelOverlay || !panelHidden"
        :id="panelId"
        :ticket="ticket"
        :query-base="base ?? ''"
        :overlay="panelOverlay"
        @close="panelOpen = false"
      >
        <template #details>
          <TicketDetails
            :ticket="ticket"
            :query-base="base ?? ''"
            :statuses="allStatuses"
            :request-types="requestTypeOptions"
            :assign-options="assignOptions"
            :assign-value="assignValue"
            :assign-loader="assignLoader"
            :can-assign="canAssign"
            :assignee-editable="assigneeEditable"
            :can-assign-self="canAssignSelf"
            :busy="busy"
            :readonly="archived"
            @status="onDetailsStatus"
            @hold="openHold"
            @reopen="transition({ action: 'reopen' })"
            @archive="archiveConfirm = true"
            @priority="(p) => patchTicket({ priority: p })"
            @request-type="(id) => patchTicket({ requestTypeId: id })"
            @organisation="(id) => patchTicket({ organisationId: id })"
            @assign="doAssign"
            @transfer="openTransfer"
            @updated="onTicketUpdated"
            @stale="onStale"
            @links-changed="base && invalidate(`${base}:activity`)"
          />
        </template>
      </TicketSidePanel>
    </Teleport>
  </div>

  <BaseDrawer v-model:open="panelOpen" side="right" label="Ticket details and activity">
    <div :id="panelTarget" class="flex min-h-0 flex-1 flex-col"></div>
  </BaseDrawer>

  <BaseModal v-model:open="transferOpen" title="Transfer ticket" description="Move the ticket to another group and optionally assign it to one of that group’s active members.">
    <form class="flex flex-col gap-3" @submit.prevent="doTransfer">
      <FormField label="Group">
        <SelectInput v-model="transferTarget" :options="transferOptions" searchable search-placeholder="Search groups" placeholder="Choose a group" />
      </FormField>
      <FormField v-if="transferTarget" label="Assignee" optional :hint="transferNote ?? 'Only active members of the chosen group are listed.'">
        <SelectInput
          :key="transferTarget"
          v-model="transferAssignee"
          :options="transferAssignee !== UNASSIGNED && ticket?.assignee?.id === transferAssignee ? [{ value: UNASSIGNED, label: 'Unassigned' }, { value: transferAssignee, label: ticket.assignee.name, avatar: 'staff' }] : [{ value: UNASSIGNED, label: 'Unassigned' }]"
          :loader="transferLoader"
          search-placeholder="Search members"
          empty-text="No active members match"
        />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="transferOpen = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :disabled="!transferTarget">Transfer</BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal
    v-model:open="releaseOpen"
    title="Release ticket?"
    :description="`The ticket goes back to the ${ticket?.group.name ?? 'group'} queue unassigned. The reason is recorded in the activity log for the team.`"
  >
    <form class="flex flex-col gap-3" novalidate @submit.prevent="doRelease">
      <BaseAlert v-if="releaseError" :error="releaseError" />
      <FormField label="Reason" required hint="3–500 characters. Shown in the ticket activity; not sent to the requester.">
        <TextArea v-model="releaseReason" :rows="3" maxlength="500" autofocus />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="releaseOpen = false">Cancel</BaseButton>
        <BaseButton type="submit" :loading="releasing" :disabled="releaseReason.trim().length < 3">Release ticket</BaseButton>
      </div>
    </form>
  </BaseModal>

  <HoldDialog v-model:open="holdOpen" :busy="holdBusy" :error="holdError" @submit="doHold" />

  <BaseModal v-model:open="archiveConfirm" title="Archive this ticket?" description="Archived tickets are permanent and cannot be reopened. Further work needs a linked follow-up ticket.">
    <template #footer>
      <BaseButton @click="archiveConfirm = false">Cancel</BaseButton>
      <BaseButton variant="danger" @click="confirmArchive"><template #icon><IconArchive :size="16" stroke="1.75" aria-hidden="true" /></template>Archive ticket</BaseButton>
    </template>
  </BaseModal>

  <ScheduleDialog
    v-if="ticket && editTarget"
    v-model:open="editScheduledOpen"
    mode="edit"
    :audience="editTarget.visibility"
    :statuses="thenStatuses"
    :current-status-id="ticket.status.id"
    :initial-send-at="editTarget.sendAt"
    :initial-then="editTarget.thenStatusId"
    :initial-body="editTarget.body"
    :busy="editScheduledBusy"
    :error="editScheduledError"
    @submit="saveScheduled"
  />

  <CopyTicketDialog v-if="ticket" v-model:open="followUpOpen" :ticket="ticket" />

  <BaseModal
    v-model:open="redactOpen"
    :title="redactFilesOnly ? 'Remove attachments?' : 'Redact message?'"
    width="md"
    :description="
      redactFilesOnly
        ? `The message text was already redacted. Its ${redactFilesLabel} can no longer be downloaded by anyone and the files are deleted. This cannot be undone. Copies already delivered by email or webhook cannot be retracted.`
        : `The message text is replaced with [redacted] for everyone, including in search and exports. This cannot be undone. Copies already delivered by email or webhook cannot be retracted.${redactKept ? ' Attachments are kept unless you also remove them below.' : ''}`
    "
  >
    <form class="flex flex-col gap-3" novalidate @submit.prevent="doRedact">
      <BaseAlert v-if="redactError" :error="redactError" />
      <p v-if="redactTarget" class="m-0 text-muted">
        {{ redactTarget.visibility === 'agent' ? 'Agent note' : 'Public message' }} from {{ redactTarget.author.name }},
        <RelativeTime :iso="redactTarget.createdAt" />.
      </p>
      <FormField label="Reason" required hint="3–1000 characters. Kept in the audit log for administrators; not shown on the message.">
        <TextArea v-model="redactReason" :rows="2" maxlength="1000" />
      </FormField>
      <CheckboxInput
        v-if="!redactFilesOnly && redactKept"
        v-model="redactFiles"
        :label="`Also remove its ${redactFilesLabel}`"
        hint="Removed files can no longer be downloaded by anyone and are deleted. This cannot be undone."
      />
      <CheckboxInput
        v-model="redactConfirm"
        :label="redactFilesOnly ? 'I understand the files cannot be recovered' : 'I understand the original text cannot be recovered'"
      />
      <div class="flex justify-end gap-2">
        <BaseButton @click="redactOpen = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="danger" :loading="redacting" :disabled="redactReason.trim().length < 3 || !redactConfirm">
          {{ redactFilesOnly ? 'Remove attachments' : 'Redact message' }}
        </BaseButton>
      </div>
    </form>
  </BaseModal>

  <span class="sr-only" aria-live="polite">{{ busy ? 'Updating ticket…' : '' }}</span>
</template>
