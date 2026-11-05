<script setup lang="ts">
// Hold reasons (admins; integrations with the holdReasons scopes). A ticket is put on hold for one of
// these reasons and returns to Open after exactly the reason's days. A new number of days affects
// only holds started afterwards; a reason cannot be deleted while a ticket is on hold with it.
import { IconPlus } from '@tabler/icons-vue'
import { computed, ref } from 'vue'
import { holdReasonsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { HoldReason } from '@/api/types'
import EntityIcon from '@/components/base/EntityIcon.vue'
import { BaseAlert, BaseButton, BaseModal, DataTable, FormField, NumberInput, TextInput, type Column } from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

const reasonsQ = useQuery(
  () => 'hold-reasons',
  (signal) => holdReasonsApi.list({ signal }),
)
const reasons = computed(() => reasonsQ.data.value?.items ?? [])

const columns: Column[] = [
  { key: 'name', label: 'Reason', class: 'font-medium' },
  { key: 'maxDays', label: 'Max days', width: '110px' },
  { key: 'inUse', label: 'In use', width: '110px' },
  { key: 'manage', label: 'Manage', width: '150px' },
]
const days = (n: number) => `${n} day${n === 1 ? '' : 's'}`

// ---- create / edit ----
const open = ref(false)
const editing = ref<HoldReason | null>(null)
const name = ref('')
const maxDays = ref(5)
const busy = ref(false)
const error = ref<unknown>(null)

function openEditor(r: HoldReason | null) {
  editing.value = r
  name.value = r?.name ?? ''
  maxDays.value = r?.maxDays ?? 5
  error.value = null
  open.value = true
}

async function save() {
  busy.value = true
  error.value = null
  const input = { name: name.value.trim(), maxDays: Number(maxDays.value) }
  try {
    if (editing.value) await holdReasonsApi.update(editing.value, input)
    else await holdReasonsApi.create(input)
    open.value = false
    invalidate('hold-reasons')
  } catch (err) {
    error.value = err
    if (err instanceof ApiError && err.is('versionConflict')) invalidate('hold-reasons')
  } finally {
    busy.value = false
  }
}
const errorText = computed(() => {
  const err = error.value
  if (err instanceof ApiError && err.fieldErrors.length) return err.fieldErrors.map((e) => e.message).join(' ')
  return err
})

// ---- delete ----
const deleting = ref<HoldReason | null>(null)
const deleteBusy = ref(false)
const deleteError = ref<unknown>(null)
const listError = ref<unknown>(null)
async function confirmDelete() {
  const r = deleting.value
  if (!r) return
  deleteBusy.value = true
  deleteError.value = null
  try {
    await holdReasonsApi.remove(r)
    deleting.value = null
  } catch (err) {
    if (err instanceof ApiError && err.is('holdReasonNotFound')) deleting.value = null
    else if (err instanceof ApiError && err.is('holdReasonInUse'))
      deleteError.value = 'A ticket is on hold with this reason. Delete it after the ticket comes off hold.'
    else deleteError.value = err
  } finally {
    invalidate('hold-reasons')
    deleteBusy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage
      title="Hold reasons"
      description="Staff put a ticket on hold for one of these reasons. It returns to Open on its own after the reason’s days; the assignee is emailed, the requester is not."
    >
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openEditor(null)">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New hold reason
        </BaseButton>
      </template>
      <BaseAlert v-if="listError" :error="listError" />
      <DataTable
        :columns="columns"
        :rows="reasons"
        :row-key="(r: HoldReason) => r.id"
        label="Hold reasons"
        :loading="reasonsQ.loading.value"
        :error="reasonsQ.error.value"
        empty-text="No hold reasons yet. Staff cannot put tickets on hold until you add one." empty-kind="holdReason"
        @retry="reasonsQ.refetch"
      >
        <template #cell-name="{ row }">
          <span class="flex items-center gap-1.5"><EntityIcon kind="holdReason" />{{ row.name }}</span>
        </template>
        <template #cell-maxDays="{ row }"><span class="lg-mono">{{ days(row.maxDays) }}</span></template>
        <template #cell-inUse="{ row }">
          <span class="lg-mono" :class="row.inUse ? '' : 'text-muted'">in use: {{ row.inUse }}</span>
        </template>
        <template #cell-manage="{ row }">
          <div class="flex gap-1.5">
            <BaseButton size="sm" aria-haspopup="dialog" @click="openEditor(row)">Edit</BaseButton>
            <BaseButton size="sm" variant="danger" aria-haspopup="dialog" @click="((deleteError = null), (deleting = row))">Delete</BaseButton>
          </div>
        </template>
      </DataTable>
    </AdminPage>
  </div>

  <BaseModal v-model:open="open" :title="editing ? 'Edit hold reason' : 'New hold reason'">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="save">
      <BaseAlert v-if="errorText" :error="errorText" />
      <FormField label="Name" required hint="Shown to staff when they put a ticket on hold. Unique in the project.">
        <TextInput v-model="name" maxlength="80" autocomplete="off" />
      </FormField>
      <FormField
        label="Maximum days"
        required
        :hint="editing ? 'A new number applies only to holds started afterwards.' : 'The ticket returns to Open after exactly this many days (1 to 365).'"
      >
        <NumberInput v-model="maxDays" :min="1" :max="365" />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!name.trim()">{{ editing ? 'Save' : 'Add hold reason' }}</BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal
    :open="deleting !== null"
    :title="`Delete “${deleting?.name ?? ''}”?`"
    :description="deleting && deleting.inUse > 0 ? `${deleting.inUse} ticket${deleting.inUse === 1 ? ' is' : 's are'} on hold with this reason, so it cannot be deleted yet.` : 'Tickets that were on hold for it keep the reason in their history. This cannot be undone.'"
    @update:open="(v) => !v && (deleting = null)"
  >
    <BaseAlert v-if="deleteError" :error="deleteError" />
    <template #footer>
      <BaseButton @click="deleting = null">Cancel</BaseButton>
      <BaseButton variant="danger" :loading="deleteBusy" @click="confirmDelete">Delete hold reason</BaseButton>
    </template>
  </BaseModal>
</template>
