<script setup lang="ts">
// "Recipients" dialog of a public reply: who sees it (the requester, and for API tickets the
// integrating system) and the ticket's CC addresses, which staff edit here. CC addresses get
// every email the requester gets (the same notices, never message bodies). Saving replaces the
// list with PATCH + If-Match, so a concurrent change is reported instead of overwritten.
import { computed, ref, watch } from 'vue'
import { ticketsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Ticket } from '@/api/types'
import { BaseAlert, BaseAvatar, BaseButton, BaseModal } from '@/components/base'
import CcEmailsInput from './CcEmailsInput.vue'

const props = defineProps<{ ticket: Ticket; disabled?: boolean }>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ updated: [ticket: Ticket]; stale: [] }>()

const draft = ref<string[]>([])
const ccInput = ref<InstanceType<typeof CcEmailsInput> | null>(null)
const error = ref<unknown>(null)
const saving = ref(false)

watch(open, (v) => {
  if (!v) return
  draft.value = [...props.ticket.ccEmails]
  ccInput.value?.reset()
  error.value = null
})

const requester = computed(() => {
  const r = props.ticket.requester
  return r.name ? `${r.name} (${r.email})` : r.email
})
const changed = computed(() => draft.value.join('\n') !== props.ticket.ccEmails.join('\n') || !!ccInput.value?.pending())

async function save() {
  if (ccInput.value?.pending() && !ccInput.value.commit()) return
  saving.value = true
  error.value = null
  try {
    emit('updated', await ticketsApi.update(props.ticket, { ccEmails: draft.value }))
    open.value = false
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      emit('stale')
      error.value = 'The ticket changed since you loaded it. It has been refreshed; your list is kept — save again.'
    } else if (err instanceof ApiError && err.is('ticketArchived')) {
      emit('stale')
      error.value = 'Archived tickets cannot be changed.'
    } else if (err instanceof ApiError && err.status === 422 && err.fieldErrors.length) {
      error.value = err.fieldErrors.map((f) => f.message).join(' ')
    } else {
      error.value = err
    }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <BaseModal v-model:open="open" title="Recipients" width="md">
    <form class="flex flex-col gap-3" @submit.prevent="save">
      <p class="m-0">
        <span class="text-muted">Visible to:</span> <span class="inline-flex items-center gap-1.5 align-bottom"><BaseAvatar :name="requester" role="requester" size="xs" decorative /><strong class="font-semibold">{{ requester }}</strong></span
        ><template v-if="ticket.source === 'api'"> via the integrating system</template>.
      </p>
      <p class="m-0 text-xs text-muted">
        The requester and every CC address receive the same email notices about this ticket (acknowledgement, reply
        notices, reminders, resolved and archived). Message text is never included in these emails.
      </p>
      <CcEmailsInput ref="ccInput" v-model="draft" :requester-email="ticket.requester.email" :disabled="disabled || saving" />
      <p v-if="!draft.length" class="m-0 text-muted">No CC addresses.</p>
      <BaseAlert v-if="error" :error="error" />
      <div class="flex justify-end gap-2 pt-1">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="saving" :disabled="disabled || !changed">Save</BaseButton>
      </div>
    </form>
  </BaseModal>
</template>
