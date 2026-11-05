<script setup lang="ts">
// "Schedule send" dialog. Quick picks are computed in the browser's time zone (in 1 hour,
// tomorrow 08:00, next Monday 08:00) plus a custom date and time. The server stores the instant
// in UTC and sends it then. Used to create (from the composer) and to edit a scheduled message
// (then the body is editable too).
import { computed, ref, watch } from 'vue'
import type { MessageVisibility, WorkflowStatus } from '@/api/types'
import { BaseAlert, BaseButton, BaseModal, DateTimeInput, FormField, MarkdownEditor, StatusSelect } from '@/components/base'
import { fullDateTime } from '@/format'

const props = defineProps<{
  mode: 'create' | 'edit'
  audience: MessageVisibility
  /** Statuses that may follow the message (empty: no status change offered). */
  statuses: WorkflowStatus[]
  currentStatusId?: string
  initialSendAt?: string | null
  initialThen?: string | null
  initialBody?: string
  busy?: boolean
  error?: unknown
}>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ submit: [value: { sendAt: string; thenStatusId: string | null; body?: string }] }>()

type Pick = 'hour' | 'tomorrow' | 'monday' | 'custom'
const pick = ref<Pick>('hour')
const custom = ref('')
const then = ref<string>('')
const body = ref('')
const now = ref(Date.now())

function at(days: number, hour: number): Date {
  const d = new Date(now.value)
  d.setDate(d.getDate() + days)
  d.setHours(hour, 0, 0, 0)
  return d
}
const quick = computed(() => {
  const hour = new Date(Math.ceil((now.value + 3600_000) / 60_000) * 60_000)
  const today = new Date(now.value).getDay() // 0 Sunday … 1 Monday
  const toMonday = ((8 - today) % 7) || 7
  return {
    hour: { label: 'In 1 hour', at: hour },
    tomorrow: { label: 'Tomorrow 08:00', at: at(1, 8) },
    monday: { label: 'Monday 08:00', at: at(toMonday, 8) },
  }
})
const sendAt = computed(() => (pick.value === 'custom' ? custom.value : quick.value[pick.value].at.toISOString()))
const minIso = computed(() => new Date(now.value + 60_000).toISOString())
const inPast = computed(() => !!sendAt.value && Date.parse(sendAt.value) <= Date.now())

// Immediate: the edit dialog is mounted already open (TicketView renders it once a message is
// chosen), so a plain watcher missed the first opening and showed defaults instead of the
// stored message, time and status.
watch(
  open,
  (v) => {
    if (!v) return
    now.value = Date.now()
    then.value = props.initialThen ?? ''
    body.value = props.initialBody ?? ''
    if (props.initialSendAt && Date.parse(props.initialSendAt) > Date.now()) {
      pick.value = 'custom'
      custom.value = props.initialSendAt
    } else {
      pick.value = 'hour'
      custom.value = ''
    }
  },
  { immediate: true },
)

const what = computed(() => (props.audience === 'public' ? 'reply' : 'note'))
const valid = computed(() => !!sendAt.value && !inPast.value && (props.mode === 'create' || !!body.value.trim()))
function submit() {
  if (!valid.value) return
  emit('submit', {
    sendAt: sendAt.value,
    thenStatusId: then.value || null,
    ...(props.mode === 'edit' ? { body: body.value } : {}),
  })
}
</script>

<template>
  <BaseModal
    v-model:open="open"
    :title="mode === 'create' ? `Schedule ${what}` : `Edit scheduled ${what}`"
    width="md"
    :description="
      mode === 'create'
        ? `The ${what} is sent at the chosen time. If the requester replies first it is held for your review instead.`
        : 'Saving marks the conversation as reviewed and keeps the message scheduled.'
    "
  >
    <form class="flex flex-col gap-3" novalidate @submit.prevent="submit">
      <BaseAlert v-if="error" :error="error" />
      <FormField v-if="mode === 'edit'" label="Message" required>
        <MarkdownEditor v-model="body" label="Message" :tone="audience === 'agent' ? 'note' : 'default'" :min-height="96" :max-height="240" />
      </FormField>
      <fieldset class="m-0 flex flex-col gap-1.5 border-0 p-0">
        <legend class="lg-label mb-1.5">Send</legend>
        <div role="radiogroup" aria-label="When to send" class="flex flex-col gap-1">
          <label v-for="(q, key) in quick" :key="key" class="lg-hov flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5">
            <input v-model="pick" type="radio" name="schedule-pick" :value="key" />
            <span class="font-medium">{{ q.label }}</span>
            <span class="text-xs text-muted">{{ fullDateTime(q.at.toISOString()) }}</span>
          </label>
          <label class="lg-hov flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5">
            <input v-model="pick" type="radio" name="schedule-pick" value="custom" />
            <span class="font-medium">Custom date and time</span>
          </label>
        </div>
      </fieldset>
      <FormField v-if="pick === 'custom'" label="Date and time" :error="inPast ? 'Choose a time in the future.' : null">
        <DateTimeInput v-model="custom" :min="minIso" />
      </FormField>
      <FormField
        v-if="statuses.length"
        label="Then set status"
        optional
        hint="Applied together with the message when it is sent, if still allowed then."
      >
        <StatusSelect v-model="then" :statuses="statuses" :current-id="currentStatusId" none-label="Keep current status" placeholder="Keep current status" />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!valid">
          {{ mode === 'create' ? 'Schedule' : 'Save' }}
        </BaseButton>
      </div>
    </form>
  </BaseModal>
</template>
