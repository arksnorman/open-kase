<script setup lang="ts">
// A scheduled reply or note shown at the end of the conversation as a distinct pending item:
// it is not part of the conversation yet (the requester sees nothing until it is sent).
// `needsReview` explains why it was held. The author can edit/reschedule, send now or cancel;
// admins can cancel other people's.
import { IconClockHour4, IconLock, IconPencil, IconSend, IconWorld } from '@tabler/icons-vue'
import { computed } from 'vue'
import type { ScheduledMessage, WorkflowStatus } from '@/api/types'
import {
  AttachmentChip,
  BaseAlert,
  BaseAvatar,
  BaseBadge,
  BaseButton,
  MarkdownView,
  RelativeTime,
  RemovedAttachmentChip,
  StatusBadge,
} from '@/components/base'

const props = defineProps<{
  item: ScheduledMessage
  statuses: WorkflowStatus[]
  /** The signed-in user wrote it (edit and send now are author-only). */
  mine: boolean
  canCancel: boolean
  busy?: boolean
  /** Archived ticket: it can no longer be sent or edited (cancel still works). */
  locked?: boolean
}>()
const emit = defineEmits<{ edit: []; 'send-now': []; cancel: [] }>()

const isNote = computed(() => props.item.visibility === 'agent')
const review = computed(() => props.item.state === 'needsReview')
const then = computed(() => props.statuses.find((s) => s.id === props.item.thenStatusId) ?? null)
const reasons: Record<string, string> = {
  newerRequesterReply: 'The requester replied after you scheduled this, so it was not sent. Review the conversation, then send it now, edit and reschedule, or cancel it.',
  statusChangeNotAllowed: 'The status change chosen for this message is no longer possible from the ticket’s current status, so it was not sent. Edit the status or send it now without one.',
  invalidMessage: 'The message no longer meets the message rules (for example a lowered size limit), so it was not sent. Edit it.',
}
const reviewText = computed(() => reasons[props.item.reason ?? ''] ?? 'This message was not sent automatically. Review it before sending.')
const overdue = computed(() => props.item.state === 'scheduled' && Date.parse(props.item.sendAt) <= Date.now())
</script>

<template>
  <article
    class="lg-msg border-dashed"
    :class="[isNote ? 'lg-msg-note' : '', review ? '!border-warn' : '']"
    :aria-label="`Scheduled ${isNote ? 'agent note' : 'public reply'} by ${item.author.name}, not sent yet`"
  >
    <div class="flex flex-wrap items-center gap-2">
      <IconClockHour4 :size="18" stroke="1.75" aria-hidden="true" class="text-muted" />
      <span class="font-semibold" :class="review ? 'text-warn' : ''">{{ review ? 'Held for review' : 'Scheduled' }}</span>
      <span class="text-muted">
        <template v-if="review">· was due <RelativeTime :iso="item.sendAt" /></template>
        <template v-else-if="overdue">· sending…</template>
        <template v-else>· <RelativeTime :iso="item.sendAt" future-prefix="sends" /></template>
      </span>
      <span v-if="!mine" class="inline-flex items-center gap-1.5 text-muted">· by <BaseAvatar :name="item.author.name" role="staff" size="xs" decorative />{{ item.author.name }}</span>
      <span class="flex-1"></span>
      <BaseBadge v-if="isNote" tone="note">
        <template #icon><IconLock :size="13" stroke="1.75" aria-hidden="true" /></template>
        Agent note
      </BaseBadge>
      <BaseBadge v-else tone="public">
        <template #icon><IconWorld :size="13" stroke="1.75" aria-hidden="true" /></template>
        Public reply
      </BaseBadge>
    </div>
    <BaseAlert v-if="review" tone="warn" :error="reviewText" />
    <MarkdownView :source="item.body" class="pl-[26px]" />
    <div v-if="item.attachments.length" class="flex flex-wrap gap-2 pl-[26px]">
      <template v-for="a in item.attachments" :key="a.id">
        <RemovedAttachmentChip v-if="a.removed" />
        <AttachmentChip v-else :filename="a.filename" :size="a.size" />
      </template>
    </div>
    <div class="flex flex-wrap items-center gap-2 pl-[26px]">
      <span v-if="then" class="inline-flex items-center gap-1.5 text-xs text-muted">Then sets status <StatusBadge :status="then" /></span>
      <span class="flex-1"></span>
      <template v-if="mine">
        <BaseButton size="sm" :disabled="busy || locked" aria-haspopup="dialog" @click="emit('edit')"><template #icon><IconPencil :size="14" stroke="1.75" aria-hidden="true" /></template>Edit…</BaseButton>
        <BaseButton size="sm" :variant="review ? 'primary' : 'secondary'" :disabled="busy || locked" @click="emit('send-now')"><template #icon><IconSend :size="14" stroke="1.75" aria-hidden="true" /></template>Send now</BaseButton>
      </template>
      <BaseButton v-if="canCancel" size="sm" variant="ghost" :disabled="busy" @click="emit('cancel')">Cancel</BaseButton>
    </div>
  </article>
</template>
