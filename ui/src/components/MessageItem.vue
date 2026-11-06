<script setup lang="ts">
import { IconArrowsMaximize, IconDotsVertical, IconLock, IconWorld } from '@tabler/icons-vue'
import { computed, ref } from 'vue'
import { ticketsApi } from '@/api/endpoints'
import { errorMessage } from '@/api/errors'
import type { Attachment, Message, Ticket } from '@/api/types'
import {
  AttachmentChip,
  BaseAvatar,
  BaseBadge,
  BaseModal,
  DropdownMenu,
  ExpandableContent,
  IconButton,
  MarkdownView,
  RelativeTime,
  RemovedAttachmentChip,
  type MenuItem,
} from '@/components/base'
import { fullDateTime } from '@/format'
import { saveBlob } from '@/download'

const props = defineProps<{
  message: Message
  ticket: Ticket
  fresh?: boolean
  /** Admins may redact (irreversible). */
  canRedact?: boolean
  /** Who redacted it, when known from the activity timeline. */
  redactedBy?: string | null
  /** The newest message of the thread: never collapsed. */
  newest?: boolean
}>()
const emit = defineEmits<{ redact: [message: Message] }>()

const redacted = computed(() => props.message.redactedAt !== null)
/** Files still downloadable (a redaction may have removed the others). */
const keptFiles = computed(() => props.message.attachments.filter((a) => !a.removed).length)
// A redacted message whose files were kept can still lose them (same dialog, attachments only).
const menu = computed<MenuItem[]>(() => {
  if (!props.canRedact) return []
  if (!redacted.value) return [{ label: 'Redact…', tone: 'danger', action: () => emit('redact', props.message) }]
  if (keptFiles.value) return [{ label: 'Remove attachments…', tone: 'danger', action: () => emit('redact', props.message) }]
  return []
})

const isNote = computed(() => props.message.visibility === 'agent')
/** Staff wear the headset, bots (tokens) the robot, the requester the person icon. */
const avatarRole = computed<'requester' | 'bot' | 'staff'>(() => (props.message.author.context === 'contact' ? 'requester' : props.message.author.context === 'bot' ? 'bot' : 'staff'))
const isRequester = computed(() => avatarRole.value === 'requester')
/** A contact of the ticket's organisation who is not the requester (replied through contactId). */
const isTeammate = computed(() => isRequester.value && props.message.author.id !== props.ticket.requester.id)
const isBot = computed(() => props.message.author.context === 'bot')
const label = computed(
  () =>
    `${isNote.value ? 'Agent note, not visible to requester,' : 'Public message'} from ${props.message.author.name}${isBot.value ? ' (bot)' : ''}, ${fullDateTime(props.message.createdAt)}`,
)

// Bodies longer than about six lines are clamped in the thread (never the newest message);
// "Show full message" expands inline and the header button opens a large reading view.
const expanded = ref(false)
const readerOpen = ref(false)

const downloadError = ref<string | null>(null)
async function download(a: Attachment) {
  downloadError.value = null
  try {
    saveBlob(await ticketsApi.downloadAttachment(a.downloadUrl), a.filename)
  } catch (err) {
    downloadError.value = `Download failed: ${errorMessage(err)}`
  }
}
</script>

<template>
  <article class="lg-msg" :class="[isNote ? 'lg-msg-note' : isRequester ? 'lg-msg-requester' : '', fresh ? '!border-warn' : '']" :aria-label="fresh ? `New: ${label}` : label">
    <!-- Wraps on a narrow thread (badges and time move below the author) instead of breaking names. -->
    <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
      <BaseAvatar :name="message.author.name" :role="avatarRole" />
      <span class="font-semibold whitespace-nowrap">{{ message.author.name }}</span>
      <BaseBadge v-if="isBot">Bot</BaseBadge>
      <BaseBadge v-if="fresh" tone="warn">New</BaseBadge>
      <span class="flex-1"></span>
      <BaseBadge v-if="isNote" tone="note">
        <template #icon><IconLock :size="13" stroke="1.75" aria-hidden="true" /></template>
        Agent note<span class="sr-only">, not visible to requester</span>
      </BaseBadge>
      <BaseBadge v-else-if="isRequester" tone="requester">{{ isTeammate ? 'Contact' : 'Requester' }}</BaseBadge>
      <BaseBadge v-else tone="public">
        <template #icon><IconWorld :size="13" stroke="1.75" aria-hidden="true" /></template>
        Public
      </BaseBadge>
      <span class="text-muted whitespace-nowrap"><RelativeTime :iso="message.createdAt" /></span>
      <span class="flex items-center">
        <IconButton v-if="!redacted" label="Open message in reading view" aria-haspopup="dialog" class="!size-7" @click="readerOpen = true">
          <IconArrowsMaximize :size="15" stroke="1.75" />
        </IconButton>
        <DropdownMenu v-if="menu.length" :items="menu" label="Message actions">
          <template #trigger="{ open, toggle }">
            <IconButton label="Message actions" :expanded="open" aria-haspopup="menu" class="!size-7" @click="toggle">
              <IconDotsVertical :size="15" stroke="1.75" />
            </IconButton>
          </template>
        </DropdownMenu>
      </span>
    </div>
    <p v-if="redacted && message.redactedAt" class="m-0 pl-[34px] text-muted">
      <span class="lg-mono">[redacted]</span>
      <span class="text-xs">
        · by {{ redactedBy ?? 'an administrator' }} · <RelativeTime :iso="message.redactedAt" /> · reason hidden
      </span>
    </p>
    <ExpandableContent
      v-else
      v-model:expanded="expanded"
      class="pl-[34px]"
      :max-height="126"
      :tolerance="24"
      :disabled="newest"
      more-label="Show full message"
      less-label="Show less"
    >
      <MarkdownView :source="message.body" />
    </ExpandableContent>
    <div v-if="message.attachments.length" class="flex flex-wrap gap-2 pl-[34px]">
      <template v-for="a in message.attachments" :key="a.id">
        <RemovedAttachmentChip v-if="a.removed" />
        <AttachmentChip v-else :filename="a.filename" :size="a.size" @download="download(a)" />
      </template>
    </div>
    <p v-if="downloadError" role="alert" class="m-0 pl-[34px] text-xs text-danger">{{ downloadError }}</p>

    <!-- Reading view: native modal dialog (Esc and × close it, focus stays inside and returns to the button). -->
    <BaseModal v-model:open="readerOpen" width="full" :title="`${isNote ? 'Agent note' : 'Public message'} from ${message.author.name}`">
      <div class="flex flex-wrap items-center gap-2 border-b border-border pb-3">
        <BaseAvatar :name="message.author.name" :role="avatarRole" />
        <span class="font-semibold">{{ message.author.name }}</span>
        <BaseBadge v-if="isBot">Bot</BaseBadge>
        <span class="flex-1"></span>
        <BaseBadge v-if="isNote" tone="note">
          <template #icon><IconLock :size="13" stroke="1.75" aria-hidden="true" /></template>
          Agent note<span class="sr-only">, not visible to requester</span>
        </BaseBadge>
        <BaseBadge v-else tone="public">
          <template #icon><IconWorld :size="13" stroke="1.75" aria-hidden="true" /></template>
          Public
        </BaseBadge>
        <span class="text-muted"><RelativeTime :iso="message.createdAt" /></span>
      </div>
      <div class="mx-auto w-full max-w-[72ch] overflow-auto text-[14.5px] leading-relaxed">
        <MarkdownView :source="message.body" />
      </div>
      <div v-if="message.attachments.length" class="mx-auto flex w-full max-w-[72ch] flex-wrap gap-2 border-t border-border pt-3">
        <template v-for="a in message.attachments" :key="a.id">
          <RemovedAttachmentChip v-if="a.removed" />
          <AttachmentChip v-else :filename="a.filename" :size="a.size" @download="download(a)" />
        </template>
      </div>
      <p v-if="downloadError" role="alert" class="m-0 text-xs text-danger">{{ downloadError }}</p>
    </BaseModal>
  </article>
</template>
