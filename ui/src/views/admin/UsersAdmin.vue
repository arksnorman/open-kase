<script setup lang="ts">
// Admin → Users: the list (each name opens the user's page, where details, role, status, groups and
// security actions are managed) and the New user dialog, which lands on the new user's page.
import { IconPlus, IconShieldCheck } from '@tabler/icons-vue'
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { adminApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { NameParts, UserSummary } from '@/api/types'
import {
  BaseAlert,
  BaseAvatar,
  BaseBadge,
  BaseButton,
  BaseModal,
  CheckboxInput,
  DataTable,
  FormField,
  PasswordInput,
  SegmentedControl,
  TextInput,
  type Column,
  type Segment,
} from '@/components/base'
import InvitationStatus from '@/components/account/InvitationStatus.vue'
import NameFields from '@/components/account/NameFields.vue'
import { usePagedQuery } from '@/composables/usePagedQuery'
import { derivedUsername, normUsername, USERNAME_HINT, usernameProblem, usernameTaken, userStatusLabel } from '@/usernames'
import AdminPage from './AdminPage.vue'

const router = useRouter()
const list = usePagedQuery<UserSummary>(
  () => 'admin:users',
  (cursor, signal) => adminApi.users(cursor, { signal }),
)
// Group names of the listed users, read from the member directory in batches of 100 IDs. Only IDs
// not seen yet are fetched, so Load more adds one request and the names already shown stay.
const groupsOf = ref(new Map<string, string[]>())
const asked = new Set<string>()
watch(
  () => list.items.value.map((u) => u.id),
  async (ids) => {
    const fresh = ids.filter((id) => !asked.has(id))
    fresh.forEach((id) => asked.add(id))
    for (let i = 0; i < fresh.length; i += 100) {
      const batch = fresh.slice(i, i + 100)
      try {
        const page = await adminApi.members({ userIds: batch, limit: 100 })
        const next = new Map(groupsOf.value)
        for (const m of page.items) next.set(m.userId, m.groups.map((g) => g.name))
        groupsOf.value = next
      } catch {
        batch.forEach((id) => asked.delete(id)) // retried with the next change of the list
      }
    }
  },
  { immediate: true },
)
const columns: Column[] = [
  { key: 'name', label: 'Name', class: 'font-medium' },
  { key: 'username', label: 'Username' },
  { key: 'email', label: 'Email' },
  { key: 'isAdmin', label: 'Role', width: '90px' },
  { key: 'status', label: 'Status', width: '200px' },
  { key: 'groups', label: 'Groups' },
]

// ---- Create (dialog) ----
const createOpen = ref(false)
type Access = 'invite' | 'password'
const accessOptions: Segment<Access>[] = [
  { value: 'invite', label: 'Send invitation email' },
  { value: 'password', label: 'Set initial password' },
]
const name = ref<NameParts>({ givenName: '', middleName: '', familyName: '' })
const email = ref('')
const username = ref('')
const access = ref<Access>('invite')
const password = ref('')
const isAdmin = ref(false)
const saving = ref(false)
const error = ref<unknown>(null)
const fe = (f: string): string | undefined => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const createUsernameError = computed(() => usernameProblem(username.value) ?? usernameTaken(error.value) ?? fe('username'))
const derived = computed(() => derivedUsername(email.value))
const canCreate = computed(
  () =>
    !!name.value.givenName.trim() &&
    !!name.value.familyName.trim() &&
    !!email.value.trim() &&
    !usernameProblem(username.value) &&
    (access.value === 'invite' || password.value.length > 0),
)

function openCreate() {
  name.value = { givenName: '', middleName: '', familyName: '' }
  email.value = username.value = password.value = ''
  access.value = 'invite'
  isAdmin.value = false
  error.value = null
  createOpen.value = true
}

async function create() {
  if (!canCreate.value) return
  saving.value = true
  error.value = null
  try {
    const middle = name.value.middleName.trim()
    const user = await adminApi.createUser({
      givenName: name.value.givenName.trim(),
      middleName: middle,
      familyName: name.value.familyName.trim(),
      email: email.value.trim(),
      ...(normUsername(username.value) ? { username: normUsername(username.value) } : {}),
      ...(access.value === 'password' ? { password: password.value } : { sendInvitation: true }),
      ...(isAdmin.value ? { isAdmin: true } : {}),
    })
    void list.refetch()
    createOpen.value = false
    await router.push({ name: 'admin-user', params: { id: user.id }, query: { created: access.value } })
  } catch (err) {
    error.value = err
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <AdminPage title="Users" description="People who sign in: agents and administrators. Requesters are contacts, not users. Open a user to edit their details, role, status and groups.">
    <template #actions>
      <BaseButton variant="primary" aria-haspopup="dialog" @click="openCreate">
        <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
        New user
      </BaseButton>
    </template>
    <DataTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(u: UserSummary) => u.id"
      label="Users"
      :loading="list.loading.value"
      :error="list.error.value"
      :has-more="list.hasMore.value"
      :loading-more="list.loadingMore.value"
      empty-text="No users."
      @retry="list.refetch"
      @load-more="list.loadMore"
    >
      <template #cell-name="{ row }">
        <span class="flex min-w-0 items-center gap-2">
          <BaseAvatar :name="row.name" role="staff" size="xs" :admin="row.isAdmin" decorative />
          <span class="min-w-0">
            <RouterLink :to="{ name: 'admin-user', params: { id: row.id } }" class="hover:underline">{{ row.name }}</RouterLink>
            <span v-if="!row.familyName" class="block text-xs font-normal text-muted">No last name yet</span>
          </span>
        </span>
      </template>
      <template #cell-username="{ row }"><span class="lg-mono break-all">{{ row.username }}</span></template>
      <template #cell-email="{ row }"><span class="lg-ell block text-muted">{{ row.email }}</span></template>
      <template #cell-isAdmin="{ row }">{{ row.isAdmin ? 'Admin' : 'Agent' }}</template>
      <template #cell-status="{ row }">
        <span class="flex flex-col">
          <span class="flex items-center gap-1.5">
            <BaseBadge :tone="row.status === 'active' ? 'success' : row.status === 'invited' ? 'warn' : 'neutral'">{{ userStatusLabel(row.status) }}</BaseBadge>
            <BaseBadge v-if="row.mfaEnabled" tone="strong" v-tooltip="'Two-step verification on'">
              <template #icon><IconShieldCheck :size="12" stroke="2" aria-hidden="true" /></template>
              2FA
            </BaseBadge>
          </span>
          <InvitationStatus v-if="row.status === 'invited'" :user-id="row.id" />
        </span>
      </template>
      <template #cell-groups="{ row }">
        <span class="lg-ell block text-muted" v-tooltip.truncated="(groupsOf.get(row.id) ?? []).join(', ')">
          {{ (groupsOf.get(row.id) ?? []).join(', ') || '—' }}
        </span>
      </template>
    </DataTable>

    <BaseModal v-model:open="createOpen" title="New user" width="md">
      <form class="flex flex-col gap-3" novalidate @submit.prevent="create">
        <BaseAlert v-if="error && !usernameTaken(error) && !(error instanceof ApiError && error.fieldErrors.length)" :error="error" />
        <NameFields v-model="name" :errors="fe" stacked />
        <div class="flex flex-col gap-3">
          <FormField label="Email" :error="fe('email')" required>
            <TextInput v-model="email" type="email" autocomplete="off" />
          </FormField>
          <FormField
            label="Username"
            optional
            :hint="`${USERNAME_HINT} Used to sign in instead of the email address.${email.trim() ? ` Left empty: “${derived}” (a number is added if taken).` : ''}`"
            :error="createUsernameError"
          >
            <TextInput v-model="username" autocomplete="off" autocapitalize="off" spellcheck="false" mono :placeholder="email.trim() ? derived : ''" />
          </FormField>
        </div>
        <div class="flex flex-col gap-1.5">
          <span class="lg-label">Access</span>
          <div><SegmentedControl v-model="access" :options="accessOptions" label="How the user gets access" size="sm" /></div>
        </div>
        <p v-if="access === 'invite'" class="m-0 text-xs text-muted">
          The user gets an email with a link to choose a password (valid 72 hours) and stays “Invited” until they accept.
          Needs outgoing email and a base URL (in Settings, under Configuration); otherwise the email is recorded as not sent and you can resend later.
          <span v-if="fe('sendInvitation')" class="lg-field-error block">{{ fe('sendInvitation') }}</span>
        </p>
        <FormField v-else label="Initial password" hint="12 to 256 characters. Share it securely; the user can change it in their account under Security." :error="fe('password')" required>
          <PasswordInput v-model="password" autocomplete="new-password" />
        </FormField>
        <CheckboxInput v-model="isAdmin" label="Administrator" hint="Can manage users, groups, settings and integrations." />
        <div class="flex justify-end gap-2">
          <BaseButton @click="createOpen = false">Cancel</BaseButton>
          <BaseButton type="submit" variant="primary" :loading="saving" :disabled="!canCreate">{{ access === 'invite' ? 'Create and invite' : 'Create user' }}</BaseButton>
        </div>
      </form>
    </BaseModal>
  </AdminPage>
</template>
