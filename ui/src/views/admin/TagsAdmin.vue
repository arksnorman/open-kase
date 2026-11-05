<script setup lang="ts">
import EntityIcon from '@/components/base/EntityIcon.vue'
// The tag catalog (admins). Agents and API tokens can only apply tags listed here (owner decision
// 2026-10-31); deleting a tag also removes it from every ticket that carries it.
import { IconPlus, IconTrash } from '@tabler/icons-vue'
import { computed, ref } from 'vue'
import { tagsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { TagCount } from '@/api/types'
import { BaseAlert, BaseBadge, BaseButton, BaseModal, DataTable, FormField, RelativeTime, TextInput, type Column } from '@/components/base'
import { invalidate, useQuery } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

const tagsQ = useQuery(
  () => 'tags',
  (signal) => tagsApi.list({ signal }),
)
const tags = computed(() => tagsQ.data.value?.items ?? [])

const columns: Column[] = [
  { key: 'tag', label: 'Tag', class: 'font-medium' },
  { key: 'count', label: 'Live tickets', width: '130px' },
  { key: 'created', label: 'Added', width: '110px' },
  { key: 'manage', label: 'Manage', width: '110px' },
]

const normalize = (raw: string) => raw.trim().toLowerCase().replace(/\s+/g, '-')
const plural = (n: number) => `${n} ticket${n === 1 ? '' : 's'}`

// ---- add ----
const open = ref(false)
const name = ref('')
const busy = ref(false)
const error = ref<unknown>(null)

function openAdd() {
  name.value = ''
  error.value = null
  open.value = true
}

async function add() {
  busy.value = true
  error.value = null
  try {
    await tagsApi.create(name.value)
    open.value = false
    invalidate('tags')
  } catch (err) {
    error.value = err
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
const deleting = ref<TagCount | null>(null)
const deleteBusy = ref(false)
const listError = ref<unknown>(null)
async function confirmDelete() {
  const t = deleting.value
  if (!t) return
  deleteBusy.value = true
  listError.value = null
  try {
    await tagsApi.remove(t.tag)
  } catch (err) {
    if (!(err instanceof ApiError && err.is('tagNotFound'))) listError.value = err
  } finally {
    invalidate('tags')
    deleteBusy.value = false
    deleting.value = null
  }
}
const deleteDescription = computed(() => {
  const t = deleting.value
  if (!t) return ''
  const from = t.count > 0 ? `It will be removed from ${plural(t.count)}` : 'No ticket carries it'
  return `${from}, and agents can no longer add it. This cannot be undone.`
})
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col overflow-auto">
    <AdminPage title="Tags" description="Agents and API tokens can only add the tags listed here to tickets. Add a tag before anyone uses it.">
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openAdd">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New tag
        </BaseButton>
      </template>
      <BaseAlert v-if="listError" :error="listError" />
      <DataTable
        :columns="columns"
        :rows="tags"
        :row-key="(t: TagCount) => t.tag"
        label="Tags"
        :loading="tagsQ.loading.value"
        :error="tagsQ.error.value"
        empty-text="No tags yet. Add the tags your team uses." empty-kind="tag"
        @retry="tagsQ.refetch"
      >
        <template #cell-tag="{ row }">
          <span class="flex items-center gap-1.5">
            <EntityIcon kind="tag" />
            <BaseBadge :to="{ name: 'queue', params: { view: 'all' }, query: { tag: row.tag } }" class="!text-text" :aria-label="`${row.tag}: show its tickets`">{{ row.tag }}</BaseBadge>
          </span>
        </template>
        <template #cell-count="{ row }"><span class="lg-mono">{{ row.count }}</span></template>
        <template #cell-created="{ row }"><span class="text-muted"><RelativeTime :iso="row.createdAt" mono /></span></template>
        <template #cell-manage="{ row }">
          <BaseButton size="sm" variant="danger" aria-haspopup="dialog" @click="deleting = row"><template #icon><IconTrash :size="14" stroke="1.75" aria-hidden="true" /></template>Delete</BaseButton>
        </template>
      </DataTable>
    </AdminPage>
  </div>

  <BaseModal v-model:open="open" title="New tag">
    <form class="flex flex-col gap-3" novalidate @submit.prevent="add">
      <BaseAlert v-if="errorText" :error="errorText" />
      <FormField label="Name" required :hint="name.trim() ? `Saved as “${normalize(name)}”. a–z, 0–9, “-” and “_”.` : 'a–z, 0–9, “-” and “_”; spaces become “-”.'">
        <TextInput v-model="name" maxlength="40" autocomplete="off" />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!name.trim()">Add tag</BaseButton>
      </div>
    </form>
  </BaseModal>

  <BaseModal :open="deleting !== null" :title="`Delete “${deleting?.tag ?? ''}”?`" :description="deleteDescription" @update:open="(v) => !v && (deleting = null)">
    <template #footer>
      <BaseButton @click="deleting = null">Cancel</BaseButton>
      <BaseButton variant="danger" :loading="deleteBusy" @click="confirmDelete"><template #icon><IconTrash :size="16" stroke="1.75" aria-hidden="true" /></template>Delete tag</BaseButton>
    </template>
  </BaseModal>
</template>
