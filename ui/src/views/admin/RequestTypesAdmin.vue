<script setup lang="ts">
// Request types (admins). The only classification a ticket carries besides priority. The first active
// type in this order is the default for new tickets; a deactivated type stays on the tickets that have
// it. The code is chosen at creation and never changes: integrations map by code.
import { IconArrowDown, IconArrowUp, IconPlayerPause, IconPlayerPlay, IconPlus } from '@tabler/icons-vue'
import { computed, nextTick, ref, watch } from 'vue'
import { requestTypesApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { RequestType } from '@/api/types'
import { BaseAlert, BaseBadge, BaseButton, BaseModal, DataTable, FormField, IconButton, TextInput, type Column } from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

const typesQ = useQuery(
  () => 'request-types:all',
  (signal) => requestTypesApi.list('all', { signal }),
)
const types = computed(() => typesQ.data.value?.items ?? [])

const columns: Column[] = [
  { key: 'name', label: 'Name', class: 'font-medium' },
  { key: 'code', label: 'Code', width: '180px' },
  { key: 'inUse', label: 'Tickets', width: '100px' },
  { key: 'order', label: 'Order', width: '110px' },
  { key: 'manage', label: 'Manage', width: '280px' },
]

const messages: Record<string, string> = {
  requestTypeExists: 'Another request type already has this name.',
  requestTypeCodeExists: 'Another request type already uses this code.',
  requestTypeLimitReached: 'A project can have at most 50 request types. Delete an unused one first.',
  requestTypeInUse: 'Tickets use this request type, so it cannot be deleted. Deactivate it instead.',
  requestTypeLastActive: 'At least one request type must stay active.',
  requestTypeNotFound: 'This request type no longer exists. The list has been refreshed.',
  requestTypeOrderConflict: 'The list changed while you were working. It has been refreshed; try again.',
  versionConflict: 'This request type was changed by someone else. The list has been refreshed; try again.',
}
/** A clear sentence for the known error codes; anything else is shown as the generic error. */
function explain(err: unknown): unknown {
  if (err instanceof ApiError) {
    for (const code of Object.keys(messages)) if (err.is(code)) return messages[code]
    if (err.fieldErrors.length) return err.fieldErrors.map((e) => e.message).join(' ')
  }
  return err
}
const stale = (err: unknown) => err instanceof ApiError && ['versionConflict', 'requestTypeOrderConflict', 'requestTypeNotFound'].some((c) => err.is(c))

// ---- create / rename ----
const open = ref(false)
const editing = ref<RequestType | null>(null)
const name = ref('')
const code = ref('')
const busy = ref(false)
const error = ref<unknown>(null)
const fe = (f: string): string | undefined => (error.value instanceof ApiError ? error.value.field(f) : undefined)

function openEditor(t: RequestType | null) {
  editing.value = t
  name.value = t?.name ?? ''
  code.value = ''
  error.value = null
  open.value = true
}

async function save() {
  busy.value = true
  error.value = null
  const n = name.value.trim()
  const c = code.value.trim()
  try {
    if (editing.value) await requestTypesApi.update(editing.value, { name: n })
    else await requestTypesApi.create(c ? { name: n, code: c } : { name: n })
    open.value = false
    invalidate('request-types')
  } catch (err) {
    // Field errors (name, code) show under their fields; other known errors read as a sentence.
    error.value = err
    if (stale(err)) invalidate('request-types')
  } finally {
    busy.value = false
  }
}
const errorText = computed(() => {
  const err = error.value
  if (err instanceof ApiError && err.fieldErrors.length && !Object.keys(messages).some((c) => err.is(c))) return null
  return err ? explain(err) : null
})

// ---- deactivate / reactivate ----
const toggling = ref<string | null>(null)
const listError = ref<unknown>(null)
async function setActive(t: RequestType, active: boolean) {
  toggling.value = t.id
  listError.value = null
  try {
    await requestTypesApi.update(t, { active })
  } catch (err) {
    listError.value = explain(err)
  } finally {
    invalidate('request-types')
    toggling.value = null
  }
}

// ---- order ----
const moving = ref(false)
let focusAfter: string | null = null
async function move(t: RequestType, dir: -1 | 1) {
  const ids = types.value.map((x) => x.id)
  const from = ids.indexOf(t.id)
  const to = from + dir
  if (from < 0 || to < 0 || to >= ids.length) return
  ;[ids[from], ids[to]] = [ids[to]!, ids[from]!]
  moving.value = true
  listError.value = null
  focusAfter = `${t.id}:${dir === -1 ? 'up' : 'down'}`
  try {
    await requestTypesApi.reorder(ids)
  } catch (err) {
    focusAfter = null
    listError.value = explain(err)
  } finally {
    invalidate('request-types')
    moving.value = false
  }
}
// Keep keyboard focus on the moved row's button once the list re-renders (or on its other arrow
// when it reached the first or last row and this one became disabled).
watch(
  () => types.value.map((t) => t.id).join(','),
  async () => {
    const want = focusAfter
    focusAfter = null
    if (!want) return
    await nextTick()
    const [id, dir] = want.split(':')
    const pick = (d: string) => document.querySelector<HTMLButtonElement>(`[data-move="${id}:${d}"]`)
    const btn = pick(dir!)
    ;(btn && !btn.disabled ? btn : pick(dir === 'up' ? 'down' : 'up'))?.focus()
  },
)

// ---- delete ----
const deleting = ref<RequestType | null>(null)
const deleteBusy = ref(false)
const deleteError = ref<unknown>(null)
async function confirmDelete() {
  const t = deleting.value
  if (!t) return
  deleteBusy.value = true
  deleteError.value = null
  try {
    await requestTypesApi.remove(t)
    deleting.value = null
  } catch (err) {
    if (err instanceof ApiError && err.is('requestTypeNotFound')) deleting.value = null
    else deleteError.value = explain(err)
  } finally {
    invalidate('request-types')
    deleteBusy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage
      title="Request types"
      description="How tickets are classified, for example Billing or Bug. The first active type in this order is the default for new tickets. A deactivated type cannot be chosen any more but stays on the tickets that have it. Integrations match types by code."
    >
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openEditor(null)">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New request type
        </BaseButton>
      </template>
      <BaseAlert v-if="listError" :error="listError" />
      <DataTable
        :columns="columns"
        :rows="types"
        :row-key="(r: RequestType) => r.id"
        label="Request types"
        :loading="typesQ.loading.value"
        :error="typesQ.error.value"
        empty-text="No request types yet."
        @retry="typesQ.refetch"
      >
        <template #cell-name="{ row }">
          <span class="flex items-center gap-1.5">
            {{ row.name }}
            <BaseBadge v-if="!row.active" tone="neutral">Inactive</BaseBadge>
          </span>
        </template>
        <template #cell-code="{ row }"><span class="lg-mono text-xs">{{ row.code }}</span></template>
        <template #cell-inUse="{ row }"><span class="lg-mono" :class="row.inUse ? '' : 'text-muted'">{{ row.inUse }}</span></template>
        <template #cell-order="{ row }">
          <div class="flex gap-1">
            <IconButton :label="`Move ${row.name} up`" tip="Move up" :data-move="`${row.id}:up`" :disabled="moving || types[0]?.id === row.id" @click="move(row, -1)">
              <IconArrowUp :size="16" stroke="1.75" />
            </IconButton>
            <IconButton :label="`Move ${row.name} down`" tip="Move down" :data-move="`${row.id}:down`" :disabled="moving || types[types.length - 1]?.id === row.id" @click="move(row, 1)">
              <IconArrowDown :size="16" stroke="1.75" />
            </IconButton>
          </div>
        </template>
        <template #cell-manage="{ row }">
          <div class="flex gap-1.5">
            <BaseButton size="sm" aria-haspopup="dialog" @click="openEditor(row)">Rename</BaseButton>
            <BaseButton v-if="!row.active" size="sm" :loading="toggling === row.id" @click="setActive(row, true)">
              <template #icon><IconPlayerPlay :size="14" stroke="1.75" aria-hidden="true" /></template>
              Reactivate
            </BaseButton>
            <BaseButton v-else size="sm" :loading="toggling === row.id" v-tooltip="'Hide from pickers; tickets that have it keep it'" @click="setActive(row, false)">
              <template #icon><IconPlayerPause :size="14" stroke="1.75" aria-hidden="true" /></template>
              Deactivate
            </BaseButton>
            <span v-if="row.inUse > 0" class="inline-flex" v-tooltip="'Tickets use this request type. Deactivate it instead.'">
              <BaseButton size="sm" variant="danger" disabled>Delete</BaseButton>
            </span>
            <BaseButton v-else size="sm" variant="danger" aria-haspopup="dialog" @click="((deleteError = null), (deleting = row))">Delete</BaseButton>
          </div>
        </template>
      </DataTable>
    </AdminPage>
  </div>

  <BaseModal v-model:open="open" :title="editing ? 'Rename request type' : 'New request type'">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="save">
      <BaseAlert v-if="errorText" :error="errorText" />
      <FormField label="Name" required :error="fe('name')" hint="Shown to staff. Unique in the project (case does not matter).">
        <TextInput v-model="name" maxlength="60" autocomplete="off" />
      </FormField>
      <FormField v-if="!editing" label="Code" optional :error="fe('code')" hint="Integrations match request types by this code. Leave it empty to derive it from the name. It cannot be changed later.">
        <TextInput v-model="code" maxlength="40" autocomplete="off" mono />
      </FormField>
      <p v-else class="m-0 text-sm text-muted">Code <span class="lg-mono text-text">{{ editing.code }}</span>. It does not change when the type is renamed.</p>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!name.trim()">{{ editing ? 'Save' : 'Add request type' }}</BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal
    :open="deleting !== null"
    :title="`Delete “${deleting?.name ?? ''}”?`"
    description="No ticket uses this request type. This cannot be undone."
    @update:open="(v) => !v && (deleting = null)"
  >
    <BaseAlert v-if="deleteError" :error="deleteError" />
    <template #footer>
      <BaseButton @click="deleting = null">Cancel</BaseButton>
      <BaseButton variant="danger" :loading="deleteBusy" @click="confirmDelete">Delete request type</BaseButton>
    </template>
  </BaseModal>
</template>
