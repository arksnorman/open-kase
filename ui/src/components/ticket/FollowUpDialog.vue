<script setup lang="ts">
import { IconArrowForwardUp } from '@tabler/icons-vue'
// "Create follow-up ticket": a new ticket linked `followUpTo` the original. Used from the
// archived ticket's actions and from a `createFollowUp` problem action (`href`).
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { newIdempotencyKey } from '@/api/client'
import { workflowApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { FollowUpInput, Priority } from '@/api/types'
import { BaseAlert, BaseButton, BaseModal, FormField, MarkdownEditor, PrioritySelect, TextInput } from '@/components/base'
import { invalidate } from '@/composables/useQuery'
import { ticketRoute } from '@/format'

const props = defineProps<{
  /** Original ticket number (shown as #number and used to build the request when no `href` is given). */
  ticketNumber: string
  /** Prefill; the server defaults to the original's subject anyway. */
  subject?: string
  priority?: Priority
  /** `createFollowUp` action href from a problem response. */
  href?: string
}>()
const open = defineModel<boolean>('open', { default: false })
const router = useRouter()

const subject = ref('')
const body = ref('')
const priority = ref<Priority | ''>(3)
const error = ref<unknown>(null)
const saving = ref(false)
let idemKey = ''

watch(open, (v) => {
  if (!v) return
  subject.value = props.subject ?? ''
  body.value = ''
  priority.value = props.priority ?? 3
  error.value = null
  idemKey = newIdempotencyKey()
})
// A changed request must not reuse the key of an earlier attempt.
watch([subject, body, priority], () => (idemKey = newIdempotencyKey()))

const fe = (f: string) => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const generalError = computed(() => (error.value instanceof ApiError && error.value.fieldErrors.length ? 'Check the highlighted fields.' : error.value))

/** Only follow hrefs that point at this API's follow-up route. */
function safeHref(href: string | undefined): string | null {
  return href && /^\/api\/v1\/tickets\/[^/]+\/follow-up$/.test(href) ? href : null
}

async function submit() {
  const input: FollowUpInput = {}
  const s = subject.value.trim()
  if (s && s !== props.subject) input.subject = s
  const b = body.value.trim()
  if (b) input.body = b
  if (priority.value) input.priority = priority.value
  saving.value = true
  error.value = null
  try {
    const href = safeHref(props.href)
    const created = href
      ? await workflowApi.followUpAt(href, input, idemKey)
      : await workflowApi.followUp(props.ticketNumber, input, idemKey)
    open.value = false
    invalidate('tickets:')
    invalidate('queue-counts')
    invalidate(`ticket:${props.ticketNumber}:links`)
    invalidate(`ticket:${props.ticketNumber}:activity`)
    await router.push(ticketRoute(created))
  } catch (err) {
    error.value = err
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <BaseModal
    v-model:open="open"
    title="Create follow-up ticket"
    width="md"
    :description="`A new ticket linked as a follow-up to #${ticketNumber}. It gets the same requester, request type and group; messages, notes, attachments, watchers, tags and the assignee are not copied. #${ticketNumber} is not changed.`"
  >
    <form class="flex flex-col gap-3" novalidate @submit.prevent="submit">
      <BaseAlert v-if="generalError" :error="generalError" />
      <FormField label="Subject" :error="fe('subject')" hint="Defaults to the original subject.">
        <TextInput v-model="subject" maxlength="200" />
      </FormField>
      <FormField label="Priority" :error="fe('priority')">
        <PrioritySelect v-model="priority" />
      </FormField>
      <FormField
        label="First message"
        optional
        :error="fe('body')"
        :hint="`Public: the requester can see it. Leave empty for “Follow-up to #${ticketNumber}.”`"
      >
        <MarkdownEditor v-model="body" label="First message" :min-height="76" :max-height="240" expandable />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="saving"><template #icon><IconArrowForwardUp :size="16" stroke="1.75" aria-hidden="true" /></template>Create follow-up ticket</BaseButton>
      </div>
    </form>
  </BaseModal>
</template>
