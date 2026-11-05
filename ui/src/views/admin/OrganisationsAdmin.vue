<script setup lang="ts">
// Organisations (admins; integrations with the organisations scopes). An integrating system serving
// several client organisations creates one here (or through the API), then links the organisation to
// its contacts and tickets. open-kase never infers an organisation, and one that a contact or a
// ticket still references cannot be deleted.
import { IconPlayerPause, IconPlayerPlay, IconPlus } from '@tabler/icons-vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { organisationsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Organisation } from '@/api/types'
import { BaseAlert, BaseBadge, BaseButton, BaseModal, DataTable, FormField, RelativeTime, SearchInput, SegmentedControl, TextInput, type Column, type Segment } from '@/components/base'
import EntityIcon from '@/components/base/EntityIcon.vue'
import { usePagedQuery } from '@/composables/usePagedQuery'
import { invalidate } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

// The search box feeds the server query after a short pause.
const search = ref('')
const q = ref('')
let timer: ReturnType<typeof setTimeout> | undefined
watch(search, (v) => {
  clearTimeout(timer)
  timer = setTimeout(() => (q.value = v.trim()), 250)
})
onBeforeUnmount(() => clearTimeout(timer))

type ActiveFilter = 'all' | 'true' | 'false'
const activeFilter = ref<ActiveFilter>('all')
const activeOptions: Segment<ActiveFilter>[] = [
  { value: 'all', label: 'All' },
  { value: 'true', label: 'Active' },
  { value: 'false', label: 'Inactive' },
]
const list = usePagedQuery<Organisation>(
  () => `organisations:list:${activeFilter.value}:${q.value}`,
  (cursor, signal) => organisationsApi.list(cursor, q.value, { signal }, activeFilter.value),
)

const columns: Column[] = [
  { key: 'name', label: 'Organisation', class: 'font-medium' },
  { key: 'active', label: 'Status', width: '100px' },
  { key: 'externalRef', label: 'External ref', width: '170px' },
  { key: 'contactCount', label: 'Contacts', width: '100px' },
  { key: 'ticketCount', label: 'Tickets', width: '100px' },
  { key: 'updatedAt', label: 'Updated', width: '120px' },
  { key: 'manage', label: 'Manage', width: '230px' },
]

// ---- create / edit ----
const open = ref(false)
const editing = ref<Organisation | null>(null)
const name = ref('')
const externalRef = ref('')
const busy = ref(false)
const error = ref<unknown>(null)
const fe = (f: string): string | undefined => (error.value instanceof ApiError ? error.value.field(f) : undefined)

function openEditor(o: Organisation | null) {
  editing.value = o
  name.value = o?.name ?? ''
  externalRef.value = o?.externalRef ?? ''
  error.value = null
  open.value = true
}

async function save() {
  busy.value = true
  error.value = null
  const input = { name: name.value.trim(), externalRef: externalRef.value.trim() }
  try {
    if (editing.value) await organisationsApi.update(editing.value, input)
    else await organisationsApi.create(input.externalRef ? input : { name: input.name })
    open.value = false
    invalidate('organisations:')
  } catch (err) {
    error.value = err
    if (err instanceof ApiError && err.is('versionConflict')) invalidate('organisations:')
  } finally {
    busy.value = false
  }
}
const errorText = computed(() => {
  const err = error.value
  if (err instanceof ApiError && err.fieldErrors.length) return null
  return err
})

// ---- deactivate / activate ----
const toggling = ref<string | null>(null)
const toggleError = ref<unknown>(null)
async function setActive(o: Organisation, active: boolean) {
  toggling.value = o.id
  toggleError.value = null
  try {
    await organisationsApi.update(o, { active })
  } catch (err) {
    toggleError.value = err
  } finally {
    invalidate('organisations:')
    toggling.value = null
  }
}
const inUse = (o: Organisation) => o.contactCount + o.ticketCount > 0

// ---- delete ----
const deleting = ref<Organisation | null>(null)
const deleteBusy = ref(false)
const deleteError = ref<unknown>(null)
const usage = (o: Organisation) => [o.contactCount ? `${o.contactCount} contact${o.contactCount === 1 ? '' : 's'}` : '', o.ticketCount ? `${o.ticketCount} ticket${o.ticketCount === 1 ? '' : 's'}` : ''].filter(Boolean).join(' and ')
async function confirmDelete() {
  const o = deleting.value
  if (!o) return
  deleteBusy.value = true
  deleteError.value = null
  try {
    await organisationsApi.remove(o)
    deleting.value = null
  } catch (err) {
    if (err instanceof ApiError && err.is('organisationNotFound')) deleting.value = null
    else if (err instanceof ApiError && err.is('organisationInUse'))
      deleteError.value = 'Contacts or tickets still reference this organisation. Deactivate it instead: it stays on existing tickets and contacts but cannot be chosen for new ones.'
    else deleteError.value = err
  } finally {
    invalidate('organisations:')
    deleteBusy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage
      title="Organisations"
      description="The customers an integrating system serves. Deactivate one to stop new links without touching existing tickets. The integration creates an organisation, then links it to its contacts and to the tickets it creates; organisations are never guessed from email addresses."
    >
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openEditor(null)">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New organisation
        </BaseButton>
      </template>
      <BaseAlert v-if="toggleError" :error="toggleError" />
      <div class="flex flex-wrap items-center gap-3">
        <div class="w-full max-w-sm">
          <SearchInput v-model="search" label="Search organisations" placeholder="Search name or external ref" maxlength="100" autocomplete="off" />
        </div>
        <SegmentedControl v-model="activeFilter" :options="activeOptions" label="Show" size="sm" />
      </div>
      <DataTable
        :columns="columns"
        :rows="list.items.value"
        :row-key="(o: Organisation) => o.id"
        label="Organisations"
        :loading="list.loading.value"
        :error="list.error.value"
        :has-more="list.hasMore.value"
        :loading-more="list.loadingMore.value"
        :empty-text="q ? 'No organisations match.' : 'No organisations yet. Integrations create them through the API, or add one here.'"
        empty-kind="organisation"
        @retry="list.refetch"
        @load-more="list.loadMore"
      >
        <template #cell-name="{ row }">
          <span class="flex items-center gap-1.5"><EntityIcon kind="organisation" />{{ row.name }}</span>
        </template>
        <template #cell-active="{ row }"><BaseBadge :tone="row.active ? 'success' : 'neutral'">{{ row.active ? 'Active' : 'Inactive' }}</BaseBadge></template>
        <template #cell-externalRef="{ row }">
          <span v-if="row.externalRef" class="lg-mono break-all text-xs">{{ row.externalRef }}</span>
          <span v-else class="text-muted">None</span>
        </template>
        <template #cell-contactCount="{ row }"><span class="lg-mono" :class="row.contactCount ? '' : 'text-muted'">{{ row.contactCount }}</span></template>
        <template #cell-ticketCount="{ row }"><span class="lg-mono" :class="row.ticketCount ? '' : 'text-muted'">{{ row.ticketCount }}</span></template>
        <template #cell-updatedAt="{ row }"><span class="text-muted"><RelativeTime :iso="row.updatedAt" /></span></template>
        <template #cell-manage="{ row }">
          <div class="flex gap-1.5">
            <BaseButton size="sm" aria-haspopup="dialog" @click="openEditor(row)">Edit</BaseButton>
            <BaseButton v-if="!row.active" size="sm" :loading="toggling === row.id" @click="setActive(row, true)">
              <template #icon><IconPlayerPlay :size="14" stroke="1.75" aria-hidden="true" /></template>
              Activate
            </BaseButton>
            <BaseButton v-else-if="inUse(row)" size="sm" :loading="toggling === row.id" v-tooltip="'Hide from pickers and block new links; existing tickets and contacts keep the name'" @click="setActive(row, false)">
              <template #icon><IconPlayerPause :size="14" stroke="1.75" aria-hidden="true" /></template>
              Deactivate
            </BaseButton>
            <BaseButton v-if="!inUse(row)" size="sm" variant="danger" aria-haspopup="dialog" @click="((deleteError = null), (deleting = row))">Delete</BaseButton>
          </div>
        </template>
      </DataTable>
    </AdminPage>
  </div>

  <BaseModal v-model:open="open" :title="editing ? 'Edit organisation' : 'New organisation'">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="save">
      <BaseAlert v-if="errorText" :error="errorText" />
      <FormField label="Name" required :error="fe('name')" hint="Unique in the project (case does not matter).">
        <TextInput v-model="name" maxlength="200" autocomplete="off" />
      </FormField>
      <FormField label="External ref" optional :error="fe('externalRef')" hint="Your own ID for this organisation, for example the customer number in your system. Unique in the project.">
        <TextInput v-model="externalRef" maxlength="128" autocomplete="off" mono />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!name.trim()">{{ editing ? 'Save' : 'Add organisation' }}</BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal
    :open="deleting !== null"
    :title="`Delete “${deleting?.name ?? ''}”?`"
    :description="deleting && usage(deleting) ? `${usage(deleting)} still reference this organisation, so it cannot be deleted yet.` : 'This cannot be undone.'"
    @update:open="(v) => !v && (deleting = null)"
  >
    <BaseAlert v-if="deleteError" :error="deleteError" />
    <template #footer>
      <BaseButton @click="deleting = null">Cancel</BaseButton>
      <BaseButton variant="danger" :loading="deleteBusy" @click="confirmDelete">Delete organisation</BaseButton>
    </template>
  </BaseModal>
</template>
