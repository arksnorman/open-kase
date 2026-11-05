<script setup lang="ts">
// Composer with an explicit audience control beside the editor. Each audience has its own
// server-side draft (GET/PUT/DELETE .../drafts/{visibility}), autosaved after a short pause,
// so switching never moves private note text into a public reply. Drafts are private to their
// author on the server and never stored in browser storage.
//
// Sending is a split button: the primary part sends (labelled by audience, plus the remembered
// "then set status" choice), the attached menu sends and sets a status in the same request
// (`thenStatusId`, atomic on the server) or opens "Schedule send…".
//
// The editor's expand button (Ctrl/Cmd+Shift+E) shows this whole composer — audience, editor,
// attachments and Send — as a large focus view; the same editor instance stays mounted.
//
// Public replies show a "Recipients" icon button at the end of the audience row: its dialog says
// who sees the reply and edits the ticket's CC addresses (copied on every requester email).
import {
  IconAlertTriangle,
  IconCheck,
  IconChevronDown,
  IconClockHour4,
  IconLock,
  IconMailCog,
  IconMessageBolt,
  IconSend,
  IconWorld,
} from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { newIdempotencyKey } from '@/api/client'
import { ticketsApi } from '@/api/endpoints'
import { ApiError, isAbortError } from '@/api/errors'
import type { Draft, MessageVisibility, PostedMessage, ScheduledMessage, Ticket, WorkflowStatus } from '@/api/types'
import {
  AttachmentChip,
  BaseAlert,
  BaseButton,
  BasePopover,
  FilePickerButton,
  IconButton,
  MarkdownEditor,
  RelativeTime,
  SegmentedControl,
  Spinner,
  SwitchInput,
  StatusBadge,
  type Segment,
} from '@/components/base'
import { useSessionStore } from '@/stores/session'
import { useWorkspaceStore } from '@/stores/workspace'
import { createStagedAttachments, MAX_ATTACHMENTS, type StagedAttachments } from '@/composables/useStagedAttachments'
import MacroPicker from './ticket/MacroPicker.vue'
import RecipientsDialog from './ticket/RecipientsDialog.vue'
import ScheduleDialog from './ticket/ScheduleDialog.vue'
import { useFocusView } from './base/focusView'

const props = defineProps<{
  ticket: Ticket
  /** Newest public message the agent has seen; sent as `expectedLastPublicId`. */
  lastPublicId: string | null
  disabled?: boolean
  /** The statuses (for "send and set status"). */
  statuses?: WorkflowStatus[]
}>()
const emit = defineEmits<{
  sent: [posted: PostedMessage]
  scheduled: [scheduled: ScheduledMessage]
  conflict: []
  stale: []
  /** The Recipients dialog saved the CC list (the updated ticket). */
  updated: [ticket: Ticket]
}>()

const AUTOSAVE_MS = 800

const composerEl = ref<HTMLElement | null>(null)
const expanded = ref(false)
useFocusView(composerEl, expanded)

const audience = ref<MessageVisibility>('public')
// The server appends the personal and workspace signatures to public replies; the text is never
// shown here, only an on/off switch (offered when either exists). Not part of drafts.
const session = useSessionStore()
const workspace = useWorkspaceStore()
const signatureAvailable = computed(() => !!session.user?.signature || !!workspace.project?.hasSignature)
const addSignature = ref(true)
const signatureFlag = () => (audience.value === 'public' && signatureAvailable.value && !addSignature.value ? { addSignature: false } : {})
const sending = ref(false)
const error = ref<unknown>(null)
const conflict = ref(false)
/** A public reply arrived (live update or refetch) while a draft had text; nothing was sent. */
const arrived = ref(false)
let ownSentId: string | null = null
const lastSentAt = ref<string | null>(null)
const attachNotice = ref<string | null>(null)
let pendingKey: string | null = null

// ---- per-audience draft state ----
interface DraftState {
  body: string
  version: number
  base: string | null
  savedAt: string | null
  status: 'loading' | 'idle' | 'saving' | 'saved' | 'error'
  saveError: string | null
  /** Set when the stored draft was written before a newer public reply. */
  stale: boolean
  /** Another window changed this draft; our text was kept and saved over it. */
  overwroteOther: boolean
  dirty: boolean
  staged: StagedAttachments
}

function blank(): DraftState {
  return {
    body: '',
    version: 0,
    base: null,
    savedAt: null,
    status: 'loading',
    saveError: null,
    stale: false,
    overwroteOther: false,
    dirty: false,
    staged: createStagedAttachments(),
  }
}

const state = reactive<Record<MessageVisibility, DraftState>>({ public: blank(), agent: blank() })
const current = computed(() => state[audience.value])
/** Archived ticket: a one-line notice instead of a disabled editor, unless a draft still has text to copy. */
const collapsed = computed(() => !!props.disabled && !state.public.body.trim() && !state.agent.body.trim())
const timers: Partial<Record<MessageVisibility, ReturnType<typeof setTimeout>>> = {}
// All draft requests of this composer run one after another so a late save can never
// overwrite a newer one or recreate a draft the server deleted on send.
let queue: Promise<unknown> = Promise.resolve()
function enqueue<T>(job: () => Promise<T>): Promise<T> {
  const run = queue.then(job, job)
  queue = run.catch(() => undefined)
  return run
}

let loadCtrl: AbortController | null = null
async function loadDrafts(ticketId: string) {
  loadCtrl?.abort()
  const ctrl = new AbortController()
  loadCtrl = ctrl
  for (const v of ['public', 'agent'] as const) {
    clearTimeout(timers[v])
    state[v].staged.clear()
    state[v] = blank()
  }
  await Promise.all(
    (['public', 'agent'] as const).map((v) =>
      enqueue(() => ticketsApi.getDraft(ticketId, v, { signal: ctrl.signal }))
        .then((d) => {
          if (ctrl.signal.aborted || props.ticket.id !== ticketId) return
          applyServerDraft(v, d)
        })
        .catch((err) => {
          if (isAbortError(err) || ctrl.signal.aborted) return
          state[v].status = 'error'
          state[v].saveError = 'Saved draft could not be loaded.'
        }),
    ),
  )
}

function applyServerDraft(v: MessageVisibility, d: Draft) {
  const s = state[v]
  s.body = d.body
  s.version = d.version
  s.base = d.baseLastPublicId
  s.savedAt = d.updatedAt
  s.status = d.version > 0 ? 'saved' : 'idle'
  s.dirty = false
  s.stale = d.version > 0 && d.body.trim() !== '' && d.baseLastPublicId !== props.lastPublicId
}

function scheduleSave(v: MessageVisibility) {
  clearTimeout(timers[v])
  timers[v] = setTimeout(() => void saveNow(v), AUTOSAVE_MS)
}

function saveNow(v: MessageVisibility): Promise<void> {
  clearTimeout(timers[v])
  timers[v] = undefined
  const ticketId = props.ticket.id
  const s = state[v]
  if (!s.dirty || props.disabled) return Promise.resolve()
  return enqueue(async () => {
    if (props.ticket.id !== ticketId || !s.dirty) return
    s.dirty = false
    const body = s.body
    s.status = 'saving'
    s.saveError = null
    try {
      if (!body.trim()) {
        if (s.version > 0) await ticketsApi.deleteDraft(ticketId, v)
        s.version = 0
        s.base = null
        s.savedAt = null
        s.status = 'idle'
        return
      }
      s.base ??= props.lastPublicId
      const input = { body, baseLastPublicId: s.base }
      let d: Draft
      try {
        d = await ticketsApi.saveDraft(ticketId, v, input, s.version)
      } catch (err) {
        if (!(err instanceof ApiError && err.is('versionConflict'))) throw err
        // Changed in another window: keep the text on screen, say so, and save over it.
        const latest = await ticketsApi.getDraft(ticketId, v)
        s.overwroteOther = latest.body !== body
        d = await ticketsApi.saveDraft(ticketId, v, input, latest.version)
      }
      if (props.ticket.id !== ticketId) return
      s.version = d.version
      s.savedAt = d.updatedAt
      s.status = s.dirty ? 'idle' : 'saved'
    } catch (err) {
      if (props.ticket.id !== ticketId) return
      s.dirty = true
      s.status = 'error'
      s.saveError =
        err instanceof ApiError && err.is('ticketArchived')
          ? 'Archived tickets cannot keep drafts.'
          : err instanceof ApiError && err.status === 0
            ? 'Draft not saved: the server could not be reached.'
            : 'Draft not saved.'
    }
  })
}

function flushAll() {
  for (const v of ['public', 'agent'] as const) if (state[v].dirty) void saveNow(v)
}

const body = computed({
  get: () => current.value.body,
  set: (v: string) => {
    const s = current.value
    if (s.body === v) return
    s.body = v
    s.dirty = true
    s.overwroteOther = false
    pendingKey = null
    if (s.status === 'saved') s.status = 'idle'
    scheduleSave(audience.value)
  },
})

watch(audience, (_, prev) => {
  pendingKey = null
  error.value = null
  attachNotice.value = null
  if (prev && state[prev].dirty) void saveNow(prev)
})
// TicketView keys this composer by ticket, so the ticket never changes in place there: leaving a
// ticket unmounts its composer, whose onBeforeUnmount saves pending text with that ticket's own
// props. Should it change in place anyway, nothing is flushed here — by now `props.ticket` is the
// next ticket, so saveNow() would write the previous ticket's text into the next ticket's draft.
watch(
  () => props.ticket.id,
  (id) => {
    audience.value = 'public'
    addSignature.value = true
    conflict.value = false
    arrived.value = false
    error.value = null
    lastSentAt.value = null
    attachNotice.value = null
    thenStatusId.value = null
    void loadDrafts(id)
  },
)

function onBeforeUnload(e: BeforeUnloadEvent) {
  const busy = (['public', 'agent'] as const).some((v) => state[v].dirty || state[v].status === 'saving')
  if (busy) {
    flushAll()
    e.preventDefault()
  }
}
onMounted(() => {
  void loadDrafts(props.ticket.id)
  window.addEventListener('beforeunload', onBeforeUnload)
})
onBeforeUnmount(() => {
  window.removeEventListener('beforeunload', onBeforeUnload)
  loadCtrl?.abort()
  flushAll()
})

// A newer public reply showed up while the agent was writing: keep the draft untouched and
// say so (the reply is shown above, marked New), like the send-time conflict.
watch(
  () => props.lastPublicId,
  (id, prev) => {
    if (!id || id === prev || id === ownSentId || sending.value) return
    const writing = (['public', 'agent'] as const).some((v) => state[v].status !== 'loading' && state[v].body.trim() !== '')
    if (writing && !conflict.value) arrived.value = true
  },
)

// ---- audience ----
const options: Segment<MessageVisibility>[] = [
  { value: 'public', label: 'Public reply', icon: IconWorld },
  { value: 'agent', label: 'Agent note', icon: IconLock, tone: 'note' },
]

const visibleTo = computed(() => {
  if (audience.value === 'agent') return 'Support staff only'
  const r = props.ticket.requester
  const who = r.name ? `${r.name} (${r.email})` : r.email
  return props.ticket.source === 'api' ? `${who} via the integrating system` : who
})
const sendLabel = computed(() => (audience.value === 'public' ? 'Send reply' : 'Add note'))
const recipientsOpen = ref(false)
const recipientsLabel = computed(() => {
  const n = props.ticket.ccEmails.length
  return n ? `Recipients (${n} CC)` : 'Recipients'
})

// ---- then set status (remembered per ticket view until changed) ----
const WORKABLE = ['open', 'waitingCustomer', 'onHold']
/** Statuses a message may move the ticket to: Open, Awaiting customer and the built-in Resolved (On hold needs a reason; never Archived). */
const thenStatuses = computed<WorkflowStatus[]>(() => {
  if (!WORKABLE.includes(props.ticket.status.category)) return []
  return (props.statuses ?? []).filter((s) => ['open', 'waitingCustomer'].includes(s.category) || (s.builtin && s.category === 'resolved'))
})
const thenStatusId = ref<string | null>(null)
/** The remembered status when it still applies (allowed now and not the current status). */
const thenStatus = computed(() => {
  const s = thenStatuses.value.find((x) => x.id === thenStatusId.value)
  return s && s.id !== props.ticket.status.id ? s : null
})
/** Menu entries: the current status is covered by "Keep current status". */
const menuStatuses = computed(() => thenStatuses.value.filter((s) => s.id !== props.ticket.status.id))
const primaryLabel = computed(() => (thenStatus.value ? `${sendLabel.value} · ${thenStatus.value.name}` : sendLabel.value))
const menuOpen = ref(false)
const menuEl = ref<HTMLElement | null>(null)
function onMenuKey(e: KeyboardEvent) {
  const items = Array.from(menuEl.value?.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([disabled])') ?? [])
  const i = items.indexOf(document.activeElement as HTMLElement)
  if (e.key === 'ArrowDown') items[(i + 1) % items.length]?.focus()
  else if (e.key === 'ArrowUp') items[(i - 1 + items.length) % items.length]?.focus()
  else if (e.key === 'Home') items[0]?.focus()
  else if (e.key === 'End') items[items.length - 1]?.focus()
  else return
  e.preventDefault()
}
/** Remembers the choice and sends with it (only remembers while there is nothing to send). */
function chooseThen(id: string | null, close: (focus?: boolean) => void) {
  thenStatusId.value = id
  close(true)
  if (canSend.value) void send()
}

// ---- schedule send ----
const scheduleOpen = ref(false)
const scheduling = ref(false)
const scheduleError = ref<unknown>(null)
function openSchedule(close: (focus?: boolean) => void) {
  close(false)
  scheduleError.value = null
  scheduleOpen.value = true
}
async function doSchedule(value: { sendAt: string; thenStatusId: string | null }) {
  if (!canSend.value) return
  const v = audience.value
  const s = state[v]
  scheduling.value = true
  scheduleError.value = null
  clearTimeout(timers[v])
  timers[v] = undefined
  try {
    await queue
    const ids = s.staged.ids()
    const sm = await ticketsApi.schedule(props.ticket.id, {
      visibility: v,
      body: s.body.trim(),
      sendAt: value.sendAt,
      expectedLastPublicId: props.lastPublicId,
      ...signatureFlag(),
      ...(value.thenStatusId ? { thenStatusId: value.thenStatusId } : {}),
      ...(ids.length ? { attachmentIds: ids } : {}),
    })
    // The server removed this audience's draft; the composer starts empty.
    resetAudience(v)
    addSignature.value = true
    thenStatusId.value = value.thenStatusId
    scheduleOpen.value = false
    emit('scheduled', sm)
  } catch (err) {
    if (err instanceof ApiError && err.is('newerReplyExists')) {
      scheduleOpen.value = false
      conflict.value = true
      emit('conflict')
    } else {
      scheduleError.value = explain(err, 'Not scheduled:')
      if (err instanceof ApiError && err.is('ticketArchived')) emit('stale')
    }
    if (s.dirty) void saveNow(v)
  } finally {
    scheduling.value = false
  }
}

/**
 * A rejected field says what to do (e.g. "…expired. Upload them again." for staged files); the
 * generic "Some fields are invalid" does not, and the composer has no field to show it under.
 */
function explain(err: unknown, prefix: string): unknown {
  return err instanceof ApiError && err.status === 422 && err.fieldErrors.length
    ? `${prefix} ${err.fieldErrors.map((f) => f.message).join(' ')}`
    : err
}

function resetAudience(v: MessageVisibility) {
  const s = state[v]
  s.body = ''
  s.version = 0
  s.base = null
  s.savedAt = null
  s.status = 'idle'
  s.dirty = false
  s.stale = false
  s.overwroteOther = false
  s.staged.clear()
  pendingKey = null
  conflict.value = false
  arrived.value = false
  attachNotice.value = null
}

// ---- attachments ----
const staged = computed(() => current.value.staged)
function onFiles(files: File[]) {
  attachNotice.value = staged.value.add(files)
}
/** Removing a file makes room again, so the "at most …" notice no longer applies. */
function removeFile(localId: number) {
  staged.value.remove(localId)
  attachNotice.value = null
}
const uploading = computed(() => staged.value.files.some((f) => f.state === 'uploading'))
const failedUploads = computed(() => staged.value.files.some((f) => f.state === 'error'))

// ---- canned replies ----
const macroOpen = ref(false)
const insertNotice = ref<string | null>(null)
/**
 * Puts the rendered canned reply into the draft of its audience (appended to any text
 * already there). The audience control switches visibly; nothing is sent.
 */
function insertMacro(text: string, visibility: MessageVisibility) {
  const switched = audience.value !== visibility
  audience.value = visibility
  const s = state[visibility]
  const next = s.body.trim() ? `${s.body.replace(/\s+$/, '')}\n\n${text}` : text
  s.body = next
  s.dirty = true
  s.overwroteOther = false
  pendingKey = null
  if (s.status === 'saved') s.status = 'idle'
  scheduleSave(visibility)
  const label = visibility === 'public' ? 'public reply' : 'agent note'
  const notice = switched
    ? `Inserted into the ${label} (the canned reply’s audience). Review it before sending.`
    : `Inserted into the ${label}. Review it before sending.`
  // After the audience watcher (which clears notices) has run.
  void nextTick(() => (insertNotice.value = notice))
}
watch(audience, () => (insertNotice.value = null))
watch(
  () => props.ticket.id,
  () => (insertNotice.value = null),
)

// ---- send ----
const canSend = computed(
  () =>
    !props.disabled &&
    !sending.value &&
    !uploading.value &&
    !failedUploads.value &&
    current.value.status !== 'loading' &&
    !!body.value.trim(),
)

async function send() {
  if (!canSend.value) return
  const v = audience.value
  const s = state[v]
  const text = s.body.trim()
  sending.value = true
  error.value = null
  pendingKey ??= newIdempotencyKey()
  clearTimeout(timers[v])
  timers[v] = undefined
  try {
    // Let an in-flight draft save finish so it cannot recreate the draft after sending.
    await queue
    const ids = s.staged.ids()
    const then = thenStatus.value?.id
    const posted = await ticketsApi.postMessage(
      props.ticket.id,
      {
        visibility: v,
        body: text,
        expectedLastPublicId: props.lastPublicId,
        ...signatureFlag(),
        ...(then ? { thenStatusId: then } : {}),
        ...(ids.length ? { attachmentIds: ids } : {}),
      },
      pendingKey,
    )
    // The server deleted this audience's draft.
    resetAudience(v)
    addSignature.value = true
    ownSentId = posted.id
    lastSentAt.value = posted.createdAt
    emit('sent', posted)
  } catch (err) {
    if (err instanceof ApiError && err.is('newerReplyExists')) {
      // Keep the draft; the agent reviews the new reply before sending again.
      conflict.value = true
      pendingKey = null
      emit('conflict')
    } else {
      error.value =
        err instanceof ApiError && err.is('invalidTransition')
          ? 'Nothing was sent: that status change is not allowed from the ticket’s current status. The ticket has been refreshed; your text is kept.'
          : explain(err, 'Not sent:')
      // Archived elsewhere: refresh so the view shows it (and its follow-up action); text is kept.
      if (err instanceof ApiError && (err.is('invalidTransition') || err.is('ticketArchived') || err.field('thenStatusId'))) {
        pendingKey = null
        emit('stale')
      }
      // A changed body gets a new idempotency key; an unchanged retry reuses it.
    }
    if (s.dirty) void saveNow(v)
  } finally {
    sending.value = false
  }
}
</script>

<template>
  <section v-if="collapsed" aria-label="Reply composer" class="flex flex-none items-center gap-2 border-t border-border bg-surface px-5 py-3 text-muted">
    <IconLock :size="16" stroke="1.75" aria-hidden="true" class="flex-none" />
    <p class="m-0">Archived tickets cannot receive replies or notes.</p>
  </section>
  <section v-else ref="composerEl" aria-label="Reply composer" class="flex flex-none flex-col gap-2.5 border-t border-border bg-surface px-5 pt-3 pb-4">
    <BaseAlert
      v-if="conflict"
      tone="warn"
      error="A newer public reply arrived while you were writing. It is shown above, marked New. Review it, then send again. Your draft is kept."
    />
    <BaseAlert v-else-if="arrived" tone="warn">
      <span class="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span class="flex-1">A new public reply arrived while you were writing. It is shown above, marked New. Review it before sending — your draft is kept.</span>
        <BaseButton size="sm" variant="ghost" @click="arrived = false">Dismiss</BaseButton>
      </span>
    </BaseAlert>
    <BaseAlert
      v-else-if="current.stale"
      tone="info"
      error="New public replies arrived after this draft was saved. Review the conversation before sending."
    />
    <BaseAlert
      v-if="current.overwroteOther"
      tone="info"
      error="This draft was also edited in another window. The text shown here was kept and saved over it."
    />
    <BaseAlert v-if="error" :error="error" />
    <BaseAlert v-if="insertNotice" tone="info" :error="insertNotice" />
    <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
      <SegmentedControl v-model="audience" :options="options" label="Message audience" />
      <!-- Read with the editor (aria-describedby); shown in the Recipients dialog for public replies. -->
      <span id="composer-visible" class="sr-only">Visible to: {{ visibleTo }}</span>
      <!-- Always present (min height) so the composer does not jump when the status changes. -->
      <span role="status" class="inline-flex min-h-5 min-w-0 flex-1 basis-40 max-sm:order-2 items-center justify-end gap-1 text-xs whitespace-nowrap text-muted">
        <template v-if="current.status === 'loading'"><Spinner :size="12" /> Loading draft…</template>
        <template v-else-if="current.status === 'saving'"><Spinner :size="12" /> Saving draft…</template>
        <template v-else-if="current.status === 'error'">
          <IconAlertTriangle :size="14" stroke="1.75" aria-hidden="true" class="text-danger" />
          <span class="text-danger">{{ current.saveError }}</span>
        </template>
        <template v-else-if="current.status === 'saved' && current.savedAt">
          <IconCheck :size="14" stroke="1.75" aria-hidden="true" /> Draft saved <RelativeTime :iso="current.savedAt" mono />
        </template>
        <template v-else-if="lastSentAt">
          <span class="inline-flex items-center gap-1 text-public"><IconCheck :size="14" stroke="1.75" aria-hidden="true" /> Sent</span>
          <RelativeTime :iso="lastSentAt" mono />
        </template>
      </span>
      <!-- End of the row; on a phone it stays beside the audience control when the status wraps. -->
      <span v-if="audience === 'public'" class="relative inline-flex max-sm:order-1">
        <IconButton :label="recipientsLabel" aria-haspopup="dialog" class="!size-8" @click="recipientsOpen = true">
          <IconMailCog :size="18" stroke="1.75" />
        </IconButton>
        <span
          v-if="ticket.ccEmails.length"
          aria-hidden="true"
          class="pointer-events-none absolute -top-0.5 -right-0.5 min-w-3.5 rounded-full bg-text px-1 text-center text-[10px] leading-[14px] font-semibold text-surface"
        >{{ ticket.ccEmails.length }}</span>
      </span>
    </div>
    <!-- Keyed by audience so each draft has its own editor history. -->
    <MarkdownEditor
      :key="`${ticket.id}:${audience}`"
      v-model="body"
      :label="audience === 'public' ? 'Public reply' : 'Agent note'"
      :tone="audience === 'agent' ? 'note' : 'default'"
      :disabled="disabled || current.status === 'loading'"
      :placeholder="disabled ? 'Archived tickets cannot receive messages.' : audience === 'public' ? 'Write a reply to the requester' : 'Write a note for support staff'"
      describedby="composer-visible"
      expandable
      expand-container
      v-model:expanded="expanded"
      @submit="send"
    />
    <div v-if="staged.files.length" class="flex flex-wrap gap-2" aria-label="Attachments to send" role="list">
      <span v-for="f in staged.files" :key="f.localId" role="listitem">
        <AttachmentChip
          :filename="f.filename"
          :size="f.size"
          removable
          :state="f.state"
          :progress="f.progress"
          :error="f.error"
          @remove="removeFile(f.localId)"
        />
      </span>
    </div>
    <p v-if="attachNotice" role="alert" class="m-0 text-xs text-danger">{{ attachNotice }}</p>
    <div class="flex flex-wrap items-center gap-x-2 gap-y-2">
      <FilePickerButton :disabled="disabled || staged.files.length >= MAX_ATTACHMENTS" @files="onFiles" />
      <BaseButton size="sm" :disabled="disabled" aria-haspopup="dialog" @click="macroOpen = true">
        <template #icon><IconMessageBolt :size="14" stroke="1.75" aria-hidden="true" /></template>
        Insert canned reply
      </BaseButton>
      <!-- Grows between the buttons and Send; on a narrow composer it moves to its own line with Send. -->
      <span class="min-w-40 flex-1 basis-48 text-xs text-muted">
        Markdown formatting · raw HTML is not allowed · Ctrl+Enter to send<template v-if="audience === 'agent'"> · files take the note's audience</template>
      </span>
      <span v-if="failedUploads" class="text-xs text-danger">Remove failed files to send without them.</span>
      <label v-if="audience === 'public' && signatureAvailable" class="inline-flex items-center gap-2 text-xs text-muted">
        <SwitchInput v-model="addSignature" label="Add signature to this reply" :disabled="disabled" />
        Signature
      </label>
      <div role="group" aria-label="Send options" class="ml-auto flex items-stretch">
        <BaseButton variant="primary" class="!rounded-r-none" :loading="sending" :disabled="!canSend" @click="send()">
          <template #icon><IconSend :size="16" stroke="1.75" aria-hidden="true" /></template>
          {{ uploading ? 'Uploading…' : primaryLabel }}
        </BaseButton>
        <BasePopover v-model:open="menuOpen" placement="top-start" panel-class="w-80 p-1">
          <template #trigger="{ open: isOpen, toggle, panelId }">
            <button
              type="button"
              class="lg-btn-primary !rounded-l-none border-l !border-l-[color-mix(in_srgb,var(--kase-accent-fg)_35%,transparent)] !px-2"
              :disabled="disabled || sending || scheduling"
              aria-haspopup="menu"
              :aria-expanded="isOpen"
              :aria-controls="isOpen ? panelId : undefined"
              aria-label="More send options: set a status, schedule"
              @click="toggle"
            >
              <IconChevronDown :size="16" stroke="1.75" aria-hidden="true" />
            </button>
          </template>
          <template #default="{ close }">
            <div ref="menuEl" role="menu" aria-label="Send options" class="flex flex-col" @keydown="onMenuKey">
              <template v-if="menuStatuses.length">
                <p class="m-0 px-2 pt-1 pb-0.5 text-xs whitespace-nowrap text-muted">
                  {{ canSend ? `${sendLabel} and set status` : 'Status to set when sending (remembered)' }}
                </p>
                <button
                  type="button"
                  role="menuitemradio"
                  :aria-checked="!thenStatus"
                  class="lg-nav lg-hov flex w-full items-center gap-2 border-0 bg-transparent text-left"
                  @click="chooseThen(null, close)"
                >
                  <IconCheck :size="14" stroke="1.75" aria-hidden="true" class="flex-none" :class="thenStatus ? 'invisible' : ''" />
                  <span class="flex-1 whitespace-nowrap">Keep current status</span>
                </button>
                <button
                  v-for="st in menuStatuses"
                  :key="st.id"
                  type="button"
                  role="menuitemradio"
                  :aria-checked="thenStatus?.id === st.id"
                  :aria-label="`${canSend ? sendLabel + ' and set status to ' : 'Set status to '}${st.name} when sending`"
                  class="lg-nav lg-hov flex w-full min-w-0 items-center gap-2 border-0 bg-transparent text-left whitespace-nowrap"
                  @click="chooseThen(st.id, close)"
                >
                  <IconCheck :size="14" stroke="1.75" aria-hidden="true" class="flex-none" :class="thenStatus?.id === st.id ? '' : 'invisible'" />
                  <StatusBadge class="max-w-full min-w-0" :status="{ id: st.id, name: st.name, category: st.category }" />
                </button>
                <hr class="my-1 border-0 border-t border-border" />
              </template>
              <p v-else class="m-0 px-2 py-1 text-xs text-muted">
                Status changes with a message are available while the ticket is open, on hold or awaiting the customer.
              </p>
              <button
                type="button"
                role="menuitem"
                class="lg-nav lg-hov flex w-full items-center gap-2 border-0 bg-transparent text-left"
                :disabled="!canSend"
                aria-haspopup="dialog"
                @click="openSchedule(close)"
              >
                <IconClockHour4 :size="14" stroke="1.75" aria-hidden="true" class="flex-none" />
                <span class="whitespace-nowrap">Schedule send…</span>
              </button>
            </div>
          </template>
        </BasePopover>
      </div>
    </div>
    <ScheduleDialog
      v-model:open="scheduleOpen"
      mode="create"
      :audience="audience"
      :statuses="thenStatuses"
      :current-status-id="ticket.status.id"
      :initial-then="thenStatus?.id ?? null"
      :busy="scheduling"
      :error="scheduleError"
      @submit="doSchedule"
    />
    <RecipientsDialog
      v-model:open="recipientsOpen"
      :ticket="ticket"
      :disabled="disabled"
      @updated="emit('updated', $event)"
      @stale="emit('stale')"
    />
    <MacroPicker
      v-model:open="macroOpen"
      :ticket="ticket"
      @insert="insertMacro"
    />
  </section>
</template>
