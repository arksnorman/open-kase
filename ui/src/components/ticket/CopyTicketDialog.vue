<script setup lang="ts">
import { IconCopy } from '@tabler/icons-vue'
// "Copy or create a follow-up ticket": one dialog for both. Copy = same details and the original
// request; follow-up = linked to the original, starting a fresh conversation. Requester, CC
// addresses and organisation are always taken from the original and shown here read-only.
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { newIdempotencyKey } from '@/api/client'
import { adminApi, tagsApi, ticketsApi, workflowApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { CopyTicketInput, FollowUpInput, GroupRef, Priority, Ticket } from '@/api/types'
import {
  BaseAlert,
  BaseButton,
  BaseModal,
  CheckboxInput,
  FormField,
  MarkdownEditor,
  PrioritySelect,
  SegmentedControl,
  SelectInput,
  TextInput,
  type SelectOption,
} from '@/components/base'
import { invalidate } from '@/composables/useQuery'
import { showToast } from '@/composables/useToasts'
import { ticketRef, ticketRoute } from '@/format'
import { useWorkspaceStore } from '@/stores/workspace'

type Mode = 'copy' | 'followUp'

const props = defineProps<{ ticket: Ticket }>()
const open = defineModel<boolean>('open', { default: false })
const router = useRouter()
const workspace = useWorkspaceStore()

const MODES: { value: Mode; label: string }[] = [
  { value: 'copy', label: 'Copy' },
  { value: 'followUp', label: 'Follow-up' },
]
const MODE_HINT: Record<Mode, string> = {
  copy: 'A new ticket with the same details and the original request',
  followUp: 'A new ticket linked to this one, starting a fresh conversation',
}

const mode = ref<Mode>('followUp')
const subject = ref('')
const groupId = ref('')
const priority = ref<Priority | ''>(3)
const requestTypeId = ref('')
const tags = ref<string[]>([])
const body = ref('')
const includeAttachments = ref(true)
const error = ref<unknown>(null)
const saving = ref(false)
let idemKey = ''

// What the form is filled from. Loaded every time the dialog opens; the form stays hidden until
// then, so another ticket's request text, or a group, request type or tag that no longer exists,
// can never be prefilled.
const ready = ref(false)
const original = ref<{ body: string; files: number; failed: boolean } | null>(null)
const groups = ref<GroupRef[]>([])
const requestTypes = ref<SelectOption<string>[]>([])
const catalogTags = ref<SelectOption<string>[]>([])
let loadToken = 0

/** The original's first public message (the "original request") and its files. */
async function loadOriginal() {
  try {
    const page = await ticketsApi.messages(String(props.ticket.number), null)
    const first = page.items.find((m) => m.visibility === 'public')
    return { body: first && !first.redactedAt ? first.body : '', files: first ? first.attachments.filter((a) => !('removed' in a)).length : 0, failed: false }
  } catch {
    return { body: '', files: 0, failed: true }
  }
}
async function loadGroups(): Promise<GroupRef[]> {
  try {
    return (await adminApi.allGroups()).filter((g) => g.active)
  } catch {
    return workspace.myGroups // may include an inactive group; the ticket's own is checked below
  }
}
async function loadAll() {
  const token = ++loadToken
  const [o, g, rt, tg] = await Promise.all([
    loadOriginal(),
    loadGroups(),
    ticketsApi.requestTypes().then((r) => r.items.map((x) => ({ value: x.id, label: x.name })), () => []),
    tagsApi.list().then((r) => r.items.map((x) => ({ value: x.tag, label: x.tag })), () => []),
  ])
  if (token !== loadToken) return
  original.value = o
  groups.value = g
  requestTypes.value = rt
  catalogTags.value = tg
  fill()
  ready.value = true
}

const requestTypeOptions = computed(() => requestTypes.value)
const tagOptions = computed(() => catalogTags.value)
const groupOptions = computed<SelectOption<string>[]>(() => groups.value.map((g) => ({ value: g.id, label: g.name })))

/** Fills the form for the chosen mode (also used when the mode is switched). Only values that are valid now are prefilled. */
function fill() {
  const t = props.ticket
  subject.value = mode.value === 'followUp' ? `Follow-up: ${t.subject}` : t.subject
  groupId.value = groups.value.some((g) => g.id === t.group.id) ? t.group.id : ''
  priority.value = t.priority
  requestTypeId.value = requestTypes.value.some((r) => r.value === t.requestType.id) ? t.requestType.id : ''
  body.value = mode.value === 'copy' ? (original.value?.body ?? '') : ''
  bodyError.value = null
  // A failed load cannot tell whether there are files: copy them only if the agent opts in.
  includeAttachments.value = !original.value?.failed
  tags.value = mode.value === 'copy' ? t.tags.filter((x) => catalogTags.value.some((c) => c.value === x)) : []
}

watch(open, (v) => {
  if (!v) return
  ready.value = false
  original.value = null
  body.value = ''
  mode.value = ['archived', 'resolved'].includes(props.ticket.status.category) ? 'followUp' : 'copy'
  error.value = null
  idemKey = newIdempotencyKey()
  void loadAll()
})
watch(mode, () => {
  if (!ready.value) return
  error.value = null
  fill()
})
// A changed request must not reuse the key of an earlier attempt.
watch([mode, subject, groupId, priority, requestTypeId, tags, body, includeAttachments], () => (idemKey = newIdempotencyKey()), { deep: true })

const bodyError = ref<string | null>(null)
const fe = (f: string) => (f === 'body' && bodyError.value ? bodyError.value : error.value instanceof ApiError ? error.value.field(f) : undefined)
const generalError = computed(() => (error.value instanceof ApiError && error.value.fieldErrors.length ? 'Check the highlighted fields.' : error.value))

const requesterLine = computed(() => {
  const r = props.ticket.requester
  return [r.name, r.email].filter(Boolean).join(' · ') || 'Not available'
})
const orgLine = computed(() => props.ticket.organisation?.name ?? '')
/** The checkbox is offered when the original has files, or when that could not be determined. */
const offerFiles = computed(() => mode.value === 'copy' && !!original.value && (original.value.files > 0 || original.value.failed))

async function submit() {
  const t = props.ticket
  error.value = null
  bodyError.value = null
  if (mode.value === 'copy' && !body.value.trim()) {
    bodyError.value = 'Enter the first message.'
    return
  }
  saving.value = true
  try {
    let created: Ticket
    const common = {
      subject: subject.value.trim(),
      ...(groupId.value ? { groupId: groupId.value } : {}),
      ...(priority.value ? { priority: priority.value } : {}),
      ...(requestTypeId.value ? { requestTypeId: requestTypeId.value } : {}),
      tags: [...tags.value],
    }
    if (mode.value === 'copy') {
      const input: CopyTicketInput = { ...common, body: body.value.trim() }
      if (offerFiles.value) input.includeAttachments = includeAttachments.value
      created = await workflowApi.copy(String(t.number), input, idemKey)
    } else {
      const input: FollowUpInput & Omit<CopyTicketInput, 'body'> = { ...common }
      const b = body.value.trim()
      if (b) input.body = b
      created = await workflowApi.followUp(String(t.number), input, idemKey)
    }
    open.value = false
    invalidate('tickets:')
    invalidate('queue-counts')
    invalidate(`ticket:${t.number}:links`)
    invalidate(`ticket:${t.number}:activity`)
    showToast({ tone: 'success', title: mode.value === 'copy' ? 'Ticket copied' : 'Follow-up created', detail: `${ticketRef(created)} · from ${ticketRef(t)}` })
    await router.push(ticketRoute(created))
  } catch (err) {
    error.value = err
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <BaseModal v-model:open="open" title="Copy or create a follow-up ticket" width="md" :description="`Start a new ticket from ${ticketRef(ticket)}. ${ticketRef(ticket)} is not changed.`">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="submit">
      <BaseAlert v-if="generalError" :error="generalError" />
      <p v-if="!ready" class="m-0 text-xs text-muted" role="status">Loading the original ticket…</p>
      <template v-else>
      <div class="flex flex-col gap-1.5">
        <SegmentedControl v-model="mode" :options="MODES" label="Create as" block />
        <p class="m-0 text-xs text-muted" aria-live="polite">{{ MODE_HINT[mode] }}</p>
      </div>
      <FormField label="Subject" :error="fe('subject')">
        <TextInput v-model="subject" maxlength="200" />
      </FormField>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <FormField label="Group" :error="fe('groupId')">
          <SelectInput v-model="groupId" :options="groupOptions" searchable search-placeholder="Search groups" placeholder="Same as the original" />
        </FormField>
        <FormField label="Priority" :error="fe('priority')">
          <PrioritySelect v-model="priority" />
        </FormField>
        <FormField label="Request type" :error="fe('requestTypeId')">
          <SelectInput v-model="requestTypeId" :options="requestTypeOptions" searchable placeholder="Same as the original" />
        </FormField>
        <FormField label="Tags" optional :error="fe('tags')">
          <SelectInput v-model="tags" :options="tagOptions" multiple placeholder="No tags" />
        </FormField>
      </div>
      <FormField
        label="First message"
        optional
        :error="fe('body')"
        :hint="mode === 'copy' ? 'Public: the requester can see it. Starts as the original request; edit it if needed.' : `Public: the requester can see it. Leave empty for “Follow-up to ${ticketRef(ticket)}.”`"
      >
        <MarkdownEditor :key="mode" v-model="body" label="First message" :min-height="76" :max-height="240" expandable />
      </FormField>
      <CheckboxInput
        v-if="offerFiles"
        v-model="includeAttachments"
        :label="original?.failed ? 'Include attachments from the original request (could not check)' : `Include attachments from the original request (${original?.files})`"
      />
      <dl class="lg-props m-0 rounded border border-border bg-subtle p-2.5 text-[13px]">
        <dt class="text-muted">Requester</dt>
        <dd class="m-0 min-w-0 wrap-anywhere">{{ requesterLine }}</dd>
        <template v-if="ticket.ccEmails.length">
          <dt class="text-muted">CC</dt>
          <dd class="m-0 min-w-0 wrap-anywhere">{{ ticket.ccEmails.join(', ') }}</dd>
        </template>
        <template v-if="orgLine">
          <dt class="text-muted">Organisation</dt>
          <dd class="m-0 min-w-0 wrap-anywhere">{{ orgLine }}</dd>
        </template>
      </dl>
      </template>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="saving" :disabled="!ready">
          <template #icon><IconCopy :size="16" stroke="1.75" aria-hidden="true" /></template>
          {{ mode === 'copy' ? 'Create copy' : 'Create follow-up' }}
        </BaseButton>
      </div>
    </form>
  </BaseModal>
</template>
