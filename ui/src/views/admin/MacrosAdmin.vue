<script setup lang="ts">
import EntityIcon from '@/components/base/EntityIcon.vue'
// Canned replies (admins): a name, a Markdown reply and the audience it is meant for.
// Agents insert one into their composer for review; nothing is ever sent automatically.
import { IconLock, IconPencil, IconPlus, IconTrash, IconWorld } from '@tabler/icons-vue'
import { computed, ref } from 'vue'
import { macrosApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Macro, MessageVisibility } from '@/api/types'
import {
  BaseAlert,
  BaseBadge,
  BaseButton,
  BaseModal,
  CheckboxInput,
  DataTable,
  FormField,
  InlineEditField,
  MarkdownEditor,
  RelativeTime,
  SegmentedControl,
  SelectInput,
  TextInput,
  type Column,
  type Segment,
  type SelectOption,
} from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'


const macrosQ = useQuery(
  () => `macros`,
  (signal) => macrosApi.list({ signal }),
)
const macros = computed(() => [...(macrosQ.data.value?.items ?? [])].sort((a, b) => a.name.localeCompare(b.name)))

const columns: Column[] = [
  { key: 'name', label: 'Name', class: 'font-medium' },
  { key: 'visibility', label: 'Audience', width: '130px' },
  { key: 'body', label: 'Reply' },
  { key: 'active', label: 'Active', width: '110px' },
  { key: 'updated', label: 'Updated', width: '100px' },
  { key: 'manage', label: 'Manage', width: '150px' },
]

// ---- editor ----
const open = ref(false)
const editing = ref<Macro | null>(null)
const name = ref('')
const body = ref('')
const visibility = ref<MessageVisibility>('public')
const active = ref(true)
const busy = ref(false)
const error = ref<unknown>(null)

const audienceOptions: Segment<MessageVisibility>[] = [
  { value: 'public', label: 'Public reply', icon: IconWorld },
  { value: 'agent', label: 'Agent note', icon: IconLock },
]

function openEditor(m: Macro | null) {
  editing.value = m
  name.value = m?.name ?? ''
  body.value = m?.body ?? ''
  visibility.value = m?.visibility ?? 'public'
  active.value = m?.active ?? true
  error.value = null
  open.value = true
}

async function save() {
  busy.value = true
  error.value = null
  const input = { name: name.value.trim(), body: body.value, visibility: visibility.value, active: active.value }
  try {
    if (editing.value) await macrosApi.update(editing.value, input)
    else await macrosApi.create(input)
    open.value = false
    invalidate(`macros`)
  } catch (err) {
    error.value = err
    if (err instanceof ApiError && err.is('versionConflict')) invalidate(`macros`)
  } finally {
    busy.value = false
  }
}
const errorText = computed(() => {
  const err = error.value
  if (err instanceof ApiError) {
    if (err.is('versionConflict')) return 'This canned reply was changed by someone else. The list has been refreshed; open it again to edit the latest version.'
    if (err.status === 422 && err.fieldErrors.length) return err.fieldErrors.map((e) => `${e.field}: ${e.message}`).join(' · ')
  }
  return err
})

// ---- delete ----
const deleting = ref<Macro | null>(null)
const deleteBusy = ref(false)
const listError = ref<unknown>(null)
async function confirmDelete() {
  const m = deleting.value
  if (!m) return
  deleteBusy.value = true
  listError.value = null
  try {
    await macrosApi.remove(m.id)
    invalidate(`macros`)
  } catch (err) {
    listError.value = err
  } finally {
    deleteBusy.value = false
    deleting.value = null
  }
}

// ---- inline edits in the list (name, active); the reply stays in the editor dialog ----
const yesNo: SelectOption<string>[] = [
  { value: 'true', label: 'Yes' },
  { value: 'false', label: 'No' },
]
/** Saves one property of a canned reply; throws the text shown beside it. */
async function saveMacro(m: Macro, input: { name?: string; active?: boolean }) {
  if (input.name !== undefined && !input.name.trim()) throw 'Enter a name.'
  try {
    await macrosApi.update(m, input.name !== undefined ? { name: input.name.trim() } : input)
    invalidate(`macros`)
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      invalidate(`macros`)
      throw 'This canned reply was changed by someone else. The list has been refreshed; try again.'
    }
    if (err instanceof ApiError && err.fieldErrors.length) throw err.fieldErrors.map((f) => f.message).join(' ')
    throw err
  }
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage
      title="Canned replies"
      description="Agents insert a canned reply into their composer and review it before sending. Nothing is ever sent automatically."
    >
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openEditor(null)">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New canned reply
        </BaseButton>
      </template>
      <BaseAlert v-if="listError" :error="listError" />
      <DataTable :columns="columns" :rows="macros" :row-key="(m: Macro) => m.id" label="Canned replies" :loading="macrosQ.loading.value" :error="macrosQ.error.value" empty-text="No canned replies yet." empty-kind="reply" @retry="macrosQ.refetch">
        <template #cell-visibility="{ row }">
          <BaseBadge :tone="row.visibility === 'agent' ? 'note' : 'public'">{{ row.visibility === 'agent' ? 'Agent note' : 'Public reply' }}</BaseBadge>
        </template>
        <template #cell-body="{ row }">
          <span class="lg-ell block text-muted" v-tooltip.truncated="row.body">{{ row.body }}</span>
        </template>
        <template #cell-name="{ row }">
          <div class="flex min-w-0 items-center gap-1.5">
            <EntityIcon kind="reply" />
            <InlineEditField class="min-w-0 flex-1" label="name" mode="text" :value="row.name" @save="(v: string) => saveMacro(row, { name: v })">
              <template #editor="{ editorRef, draft, setDraft, invalid, errorId }">
                <TextInput :ref="editorRef" :model-value="draft" size="sm" maxlength="80" :aria-label="`Name of ${row.name}`" :aria-describedby="errorId" :invalid="invalid" @update:model-value="setDraft" />
              </template>
            </InlineEditField>
          </div>
        </template>
        <template #cell-active="{ row }">
          <InlineEditField label="active" :value="row.active ? 'true' : 'false'" :display="row.active ? 'Yes' : 'No'" @save="(v: string) => saveMacro(row, { active: v === 'true' })">
            <template #editor="{ editorRef, draft, commit, closed, errorId }">
              <SelectInput :ref="editorRef" :model-value="draft" :options="yesNo" size="sm" :aria-label="`${row.name} active`" :aria-describedby="errorId" @update:model-value="commit" @close="closed" />
            </template>
          </InlineEditField>
        </template>
        <template #cell-updated="{ row }"><span class="text-muted"><RelativeTime :iso="row.updatedAt" mono /></span></template>
        <template #cell-manage="{ row }">
          <span class="flex gap-1">
            <BaseButton size="sm" aria-haspopup="dialog" @click="openEditor(row)"><template #icon><IconPencil :size="14" stroke="1.75" aria-hidden="true" /></template>Edit…</BaseButton>
            <BaseButton size="sm" variant="danger" @click="deleting = row"><template #icon><IconTrash :size="14" stroke="1.75" aria-hidden="true" /></template>Delete</BaseButton>
          </span>
        </template>
      </DataTable>
    </AdminPage>
  </div>

  <BaseModal v-model:open="open" :title="editing ? `Edit “${editing.name}”` : 'New canned reply'" width="lg">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="save">
      <BaseAlert v-if="errorText" :error="errorText" />
      <FormField label="Name" required>
        <TextInput v-model="name" maxlength="80" />
      </FormField>
      <div class="flex flex-col gap-1.5">
        <span class="lg-label" aria-hidden="true">Intended audience</span>
        <div><SegmentedControl v-model="visibility" :options="audienceOptions" label="Intended audience" size="sm" /></div>
      </div>
      <FormField id="macro-body" label="Reply" required hint="Variables: {{requester.name}}, {{ticket.number}} (shown as #1042), {{agent.name}}. Values are escaped for Markdown.">
        <MarkdownEditor id="macro-body" v-model="body" label="Reply" :tone="visibility === 'agent' ? 'note' : 'default'" :min-height="120" :max-height="280" />
      </FormField>
      <CheckboxInput v-model="active" label="Active" hint="Inactive canned replies are hidden from agents." />
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!name.trim() || !body.trim()">
          {{ editing ? 'Save changes' : 'Create canned reply' }}
        </BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal :open="deleting !== null" :title="`Delete “${deleting?.name ?? ''}”?`" description="Agents can no longer insert it. Tickets are not affected." @update:open="(v) => !v && (deleting = null)">
    <template #footer>
      <BaseButton @click="deleting = null">Cancel</BaseButton>
      <BaseButton variant="danger" :loading="deleteBusy" @click="confirmDelete"><template #icon><IconTrash :size="16" stroke="1.75" aria-hidden="true" /></template>Delete</BaseButton>
    </template>
  </BaseModal>
</template>
