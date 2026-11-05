<script setup lang="ts">
import EntityIcon from '@/components/base/EntityIcon.vue'
import { IconChevronDown, IconPlayerPause, IconPlayerPlay, IconPlus, IconUsers } from '@tabler/icons-vue'
import { ref, shallowRef } from 'vue'
import { adminApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Group } from '@/api/types'
import { BaseAvatar, BaseBadge, BaseAlert, BaseButton, BaseModal, DataTable, FormField, InlineEditField, LinkButton, TextInput, type Column } from '@/components/base'
import { usePagedQuery } from '@/composables/usePagedQuery'
import { invalidate } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

const list = usePagedQuery<Group>(
  () => 'admin:groups',
  (cursor, signal) => adminApi.groups(cursor, { signal }),
)
// Members are managed on each user's page; here a group's members are listed read-only (loaded when
// its row is expanded), linking there.
interface Roster {
  loading: boolean
  error: unknown
  members: { id: string; name: string; active: boolean }[]
  more: boolean
}
const rosters = shallowRef(new Map<string, Roster>())
async function toggleRoster(groupId: string) {
  if (rosters.value.has(groupId)) {
    rosters.value.delete(groupId)
    rosters.value = new Map(rosters.value)
    return
  }
  const r: Roster = { loading: true, error: null, members: [], more: false }
  rosters.value = new Map(rosters.value).set(groupId, r)
  const put = (patch: Partial<Roster>) => {
    if (rosters.value.get(groupId) !== r) return
    const next = { ...r, ...patch }
    Object.assign(r, next)
    rosters.value = new Map(rosters.value).set(groupId, r)
  }
  try {
    const page = await adminApi.members({ groupId, limit: 100 })
    put({
      loading: false,
      members: page.items.map((m) => ({ id: m.userId, name: m.user.name, active: m.active })).sort((a, b) => a.name.localeCompare(b.name)),
      more: !!page.nextCursor,
    })
  } catch (err) {
    put({ loading: false, error: err })
  }
}
// Fixed-width side columns; Description takes whatever the page has left (the table's minimum
// width is 820px, so it fits beside the sidebar without a horizontal scrollbar).
const columns: Column[] = [
  { key: 'name', label: 'Name', width: '210px', class: 'font-medium' },
  { key: 'description', label: 'Description', class: 'text-muted' },
  { key: 'memberCount', label: 'Members', width: '200px' },
  { key: 'active', label: 'Status', width: '150px' },
  { key: 'actions', label: 'Actions', width: '130px' },
]
const toggleError = ref<unknown>(null)
const toggling = ref<string | null>(null)

/** Deactivates or reactivates a group (Triage is protected). */
async function setActive(g: Group, active: boolean) {
  toggling.value = g.id
  toggleError.value = null
  try {
    await adminApi.updateGroup(g, { active })
    invalidate('admin:groups')
    invalidate('admin:members')
  } catch (err) {
    toggleError.value = err instanceof ApiError && err.is('versionConflict') ? 'Someone else changed this group. The list has been refreshed; try again.' : err
    void list.refetch()
  } finally {
    toggling.value = null
  }
}

const createOpen = ref(false)
const name = ref('')
const description = ref('')
const saving = ref(false)
const error = ref<unknown>(null)
const fe = (f: string) => (error.value instanceof ApiError ? error.value.field(f) : undefined)

/** Renames or describes a group; a stale version reloads the list and keeps the typed text. */
async function saveGroup(g: Group, field: 'name' | 'description', raw: string) {
  try {
    await adminApi.updateGroup(g, { [field]: raw.trim() })
    invalidate('admin:groups')
    // User lists show group names.
    invalidate('admin:members')
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      await list.refetch()
      throw new Error('Someone else changed this group. The latest version is loaded; save again to apply your text.')
    }
    throw err
  }
}

function openCreate() {
  name.value = description.value = ''
  error.value = null
  createOpen.value = true
}

async function create() {
  saving.value = true
  error.value = null
  try {
    await adminApi.createGroup({
      name: name.value.trim(),
      ...(description.value.trim() ? { description: description.value.trim() } : {}),
    })
    name.value = description.value = ''
    createOpen.value = false
    invalidate('admin:groups')
  } catch (err) {
    error.value = err
  } finally {
    saving.value = false
  }
}

</script>

<template>
  <AdminPage title="Groups" description="Groups are the team queues. Agents work the tickets of the groups they belong to. Triage is seeded and protected: it can be renamed but not deactivated or deleted. Add or remove a user's groups on their page under Users.">
      <template #actions>
        <BaseButton variant="primary" aria-haspopup="dialog" @click="openCreate">
          <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
          New group
        </BaseButton>
      </template>
      <BaseAlert v-if="toggleError" :error="toggleError" />
      <DataTable
        :columns="columns"
        :rows="list.items.value"
        :row-key="(g: Group) => g.id"
        label="Groups"
        :loading="list.loading.value"
        :error="list.error.value"
        :has-more="list.hasMore.value"
        :loading-more="list.loadingMore.value"
        empty-text="No groups."
        empty-kind="group"
        @retry="list.refetch"
        @load-more="list.loadMore"
      >
        <template #cell-name="{ row }">
          <div class="flex min-w-0 items-center gap-1.5">
            <EntityIcon kind="group" />
            <InlineEditField class="min-w-0 flex-1" label="group name" mode="text" :value="row.name" @save="(v: string) => saveGroup(row, 'name', v)">
              <template #editor="{ editorRef, draft, setDraft, invalid, errorId }">
                <TextInput :ref="editorRef" :model-value="draft" size="sm" autocomplete="off" :aria-label="`Name of group ${row.name}`" :aria-describedby="errorId" :invalid="invalid" @update:model-value="setDraft" />
              </template>
            </InlineEditField>
          </div>
        </template>
        <template #cell-description="{ row }">
          <InlineEditField label="group description" mode="text" :value="row.description ?? ''" @save="(v: string) => saveGroup(row, 'description', v)">
            <template #editor="{ editorRef, draft, setDraft, invalid, errorId }">
              <TextInput :ref="editorRef" :model-value="draft" size="sm" autocomplete="off" :aria-label="`Description of group ${row.name}`" :aria-describedby="errorId" :invalid="invalid" @update:model-value="setDraft" />
            </template>
          </InlineEditField>
        </template>
        <template #cell-memberCount="{ row }">
          <div class="flex flex-col gap-0.5">
            <span class="flex items-center whitespace-nowrap gap-x-3 gap-y-0.5">
              <span class="flex items-center gap-1.5" v-tooltip="`${row.memberCount} member(s)`">
                <IconUsers :size="16" stroke="1.75" aria-hidden="true" class="text-muted" />
                {{ row.memberCount }}
              </span>
              <LinkButton v-if="row.memberCount" tone="muted" class="inline-flex items-center gap-0.5 text-xs" :aria-label="`${rosters.has(row.id) ? 'Hide' : 'Show'} members of ${row.name}`" :aria-expanded="rosters.has(row.id)" @click="toggleRoster(row.id)">
                <IconChevronDown :size="14" stroke="1.75" aria-hidden="true" class="transition-transform motion-reduce:transition-none" :class="rosters.has(row.id) ? 'rotate-180' : ''" />
                {{ rosters.has(row.id) ? 'Hide' : 'Show' }}
              </LinkButton>
            </span>
            <template v-if="rosters.get(row.id)">
              <span v-if="rosters.get(row.id)!.loading" class="text-xs text-muted">Loading members…</span>
              <span v-else-if="rosters.get(row.id)!.error" class="text-xs text-danger">Could not load the members.</span>
              <ul v-else class="m-0 flex list-none flex-col gap-1 p-0 text-xs text-muted" aria-label="Members">
                <li v-for="m in rosters.get(row.id)!.members" :key="m.id" class="flex min-w-0 items-center gap-1.5">
                  <BaseAvatar :name="m.name" role="staff" size="xs" decorative />
                  <RouterLink :to="{ name: 'admin-user', params: { id: m.id } }" class="lg-ell hover:text-text hover:underline">{{ m.name }}</RouterLink><span v-if="!m.active" class="flex-none">(inactive)</span>
                </li>
                <li v-if="rosters.get(row.id)!.more">and more</li>
              </ul>
            </template>
          </div>
        </template>
        <template #cell-active="{ row }">
          <span class="flex flex-wrap items-center gap-1">
            <BaseBadge :tone="row.active ? 'success' : 'neutral'">{{ row.active ? 'Active' : 'Inactive' }}</BaseBadge>
            <BaseBadge v-if="row.protected" v-tooltip="'Seeded group: cannot be deactivated or deleted'">Protected</BaseBadge>
          </span>
        </template>
        <template #cell-actions="{ row }">
          <BaseButton v-if="!row.protected" size="sm" :loading="toggling === row.id" @click="setActive(row, !row.active)">
            <template #icon><component :is="row.active ? IconPlayerPause : IconPlayerPlay" :size="14" stroke="1.75" aria-hidden="true" /></template>
            {{ row.active ? 'Deactivate' : 'Activate' }}
          </BaseButton>
        </template>
      </DataTable>
      <BaseModal v-model:open="createOpen" title="New group" width="md">
        <form class="flex flex-col gap-3" novalidate @submit.prevent="create">
          <BaseAlert v-if="error && !(error instanceof ApiError && error.fieldErrors.length)" :error="error" />
          <FormField label="Name" :error="fe('name')" required>
            <TextInput v-model="name" autocomplete="off" />
          </FormField>
          <FormField label="Description" optional :error="fe('description')">
            <TextInput v-model="description" autocomplete="off" />
          </FormField>
          <div class="flex justify-end gap-2">
            <BaseButton @click="createOpen = false">Cancel</BaseButton>
            <BaseButton type="submit" variant="primary" :loading="saving" :disabled="!name.trim()">Create group</BaseButton>
          </div>
        </form>
      </BaseModal>

  </AdminPage>
</template>
