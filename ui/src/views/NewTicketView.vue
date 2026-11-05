<script setup lang="ts">
import CcEmailsInput from '@/components/ticket/CcEmailsInput.vue'
import { ticketRoute } from '@/format'
import { computed, onBeforeUnmount, ref } from 'vue'
import { useRouter } from 'vue-router'
import { newIdempotencyKey } from '@/api/client'
import { ticketsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Priority } from '@/api/types'
import {
  AttachmentChip,
  BaseAlert,
  BaseButton,
  BaseCard,
  FilePickerButton,
  FormField,
  MarkdownEditor,
  PrioritySelect,
  SelectInput,
  BaseAvatar,
  TextInput,
} from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'
import { createStagedAttachments, MAX_ATTACHMENTS } from '@/composables/useStagedAttachments'
import { loadOrganisations } from '@/organisations'
import { DEFAULT_QUEUE } from '@/queues'
import { useWorkspaceStore } from '@/stores/workspace'

const workspace = useWorkspaceStore()
const router = useRouter()

const subject = ref('')
const body = ref('')
const requesterName = ref('')
const requesterEmail = ref('')
const externalId = ref('')
const organisationId = ref<string>('')
const ccEmails = ref<string[]>([])
const ccInput = ref<InstanceType<typeof CcEmailsInput> | null>(null)
const priority = ref<Priority | ''>(3)
const groupId = ref<string>('')
const requestTypeId = ref<string>('')
const requestTypesQ = useQuery(
  () => `request-types`,
  (signal) => ticketsApi.requestTypes({ signal }),
)
const requestTypeOptions = computed(() => (requestTypesQ.data.value?.items ?? []).map((r) => ({ value: r.id, label: r.name })))
const staged = createStagedAttachments()
const attachNotice = ref<string | null>(null)
const uploading = computed(() => staged.files.some((f) => f.state === 'uploading'))
const failedUploads = computed(() => staged.files.some((f) => f.state === 'error'))
onBeforeUnmount(() => staged.clear())
const submitting = ref(false)
const error = ref<unknown>(null)
// One key per form fill so a retried submit after a network failure cannot create duplicates.
let idemKey = newIdempotencyKey()

const groupOptions = computed(() => workspace.myGroups.map((g) => ({ value: g.id, label: g.name })))
const fe = (f: string) => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const generalError = computed(() => {
  const err = error.value
  if (!err) return null
  if (err instanceof ApiError && err.fieldErrors.length) return 'Check the highlighted fields.'
  return err
})
const backTo = computed(() => ({ name: 'queue', params: { view: DEFAULT_QUEUE } }))

async function submit() {
  if (submitting.value || uploading.value || failedUploads.value) return
  // Add any address still typed in the CC box; stop if it is invalid (the box shows why).
  if (ccInput.value?.pending() && !ccInput.value.commit()) return
  submitting.value = true
  error.value = null
  try {
    const t = await ticketsApi.create(
      {
        subject: subject.value.trim(),
        body: body.value,
        requester: {
          name: requesterName.value.trim(),
          email: requesterEmail.value.trim(),
          ...(externalId.value.trim() ? { externalId: externalId.value.trim() } : {}),
        },
        ...(priority.value ? { priority: priority.value } : {}),
        ...(groupId.value ? { groupId: groupId.value } : {}),
        ...(requestTypeId.value ? { requestTypeId: requestTypeId.value } : {}),
        ...(staged.ids().length ? { attachmentIds: staged.ids() } : {}),
        ...(organisationId.value ? { organisationId: organisationId.value } : {}),
        ...(ccEmails.value.length ? { ccEmails: ccEmails.value } : {}),
      },
      idemKey,
    )
    idemKey = newIdempotencyKey()
    invalidate('tickets:')
    invalidate('queue-counts')
    await router.replace(ticketRoute(t))
  } catch (err) {
    error.value = err
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="flex h-[52px] flex-none items-center gap-4 border-b border-border bg-surface px-5">
    <nav aria-label="Breadcrumb" class="flex items-center gap-1.5 text-muted">
      <RouterLink :to="backTo" class="lg-link">{{ workspace.name }}</RouterLink>
      <span aria-hidden="true">/</span>
      <span aria-current="page" class="font-semibold text-text">New ticket</span>
    </nav>
  </div>
  <div class="flex-1 overflow-auto p-5">
    <BaseCard as="div" class="max-w-3xl !gap-4 !p-5">
      <form class="flex flex-col gap-4" novalidate @submit.prevent="submit">
        <h1 class="m-0 text-lg font-semibold">New ticket</h1>
        <BaseAlert v-if="generalError" :error="generalError" />

        <fieldset class="m-0 grid grid-cols-2 gap-3 border-0 p-0">
          <legend class="lg-h3 mb-2 flex items-center gap-2"><BaseAvatar name="Requester" role="requester" size="xs" decorative />Requester</legend>
          <FormField label="Name" :error="fe('requester.name')" required>
            <TextInput v-model="requesterName" autocomplete="off" />
          </FormField>
          <FormField label="Email" :error="fe('requester.email')" required>
            <TextInput v-model="requesterEmail" type="email" autocomplete="off" />
          </FormField>
          <FormField label="External ID" optional :error="fe('requester.externalId')" class="col-span-2">
            <TextInput v-model="externalId" autocomplete="off" mono />
          </FormField>
          <CcEmailsInput ref="ccInput" v-model="ccEmails" :requester-email="requesterEmail" :error="fe('ccEmails')" class="col-span-2" />
        </fieldset>

        <FormField label="Subject" :error="fe('subject')" required>
          <TextInput v-model="subject" maxlength="200" />
        </FormField>

        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <FormField label="Priority" :error="fe('priority')">
            <PrioritySelect v-model="priority" />
          </FormField>
          <FormField label="Request type" :error="fe('requestTypeId')">
            <SelectInput v-model="requestTypeId" :options="requestTypeOptions" placeholder="Default (Issue)" clearable searchable />
          </FormField>
          <FormField label="Group" :error="fe('groupId')">
            <SelectInput v-model="groupId" :options="groupOptions" placeholder="Default (Triage)" clearable searchable />
          </FormField>
          <FormField label="Organisation" optional :error="fe('organisationId')" class="sm:col-span-3">
            <SelectInput
              v-model="organisationId"
              :loader="loadOrganisations"
              searchable
              clearable
              placeholder="None"
              search-placeholder="Search organisations"
              empty-text="No organisations match"
            />
          </FormField>
        </div>

        <FormField label="Description" hint="Public: the requester and the integrating system can see it." :error="fe('body')">
          <MarkdownEditor v-model="body" label="Description" placeholder="Describe the request" :min-height="140" expandable @submit="submit" />
        </FormField>

        <div class="flex flex-col gap-2">
          <div v-if="staged.files.length" class="flex flex-wrap gap-2" role="list" aria-label="Attachments">
            <span v-for="f in staged.files" :key="f.localId" role="listitem">
              <AttachmentChip
                :filename="f.filename"
                :size="f.size"
                removable
                :state="f.state"
                :progress="f.progress"
                :error="f.error"
                @remove="staged.remove(f.localId)"
              />
            </span>
          </div>
          <p v-if="attachNotice" role="alert" class="m-0 text-xs text-danger">{{ attachNotice }}</p>
          <p v-if="fe('attachmentIds')" role="alert" class="m-0 text-xs text-danger">{{ fe('attachmentIds') }}</p>
          <div class="flex items-center gap-2">
            <FilePickerButton :disabled="staged.files.length >= MAX_ATTACHMENTS" @files="(f) => (attachNotice = staged.add(f))" />
            <span class="text-xs text-muted">Up to {{ MAX_ATTACHMENTS }} files, public with the description.</span>
            <span v-if="failedUploads" class="text-xs text-danger">Remove failed files to continue.</span>
          </div>
        </div>

        <div class="flex justify-end gap-2">
          <BaseButton :to="backTo">Cancel</BaseButton>
          <BaseButton type="submit" variant="primary" :loading="submitting" :disabled="!subject.trim() || !requesterName.trim() || !requesterEmail.trim() || uploading || failedUploads">
            Create ticket
          </BaseButton>
        </div>
      </form>
    </BaseCard>
  </div>
</template>
