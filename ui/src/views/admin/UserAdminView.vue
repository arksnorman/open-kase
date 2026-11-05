<script setup lang="ts">
import { IconArrowLeft, IconBan, IconKey, IconPlayerPause, IconPlayerPlay, IconSend, IconShieldOff } from '@tabler/icons-vue'
// Admin → Users → one user: details (name parts, username), role, status, groups and the security
// actions the API supports (set password, reset two-step verification, invitations).
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { adminApi, deliveriesApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Group, NameParts, UserSummary } from '@/api/types'
import {
  BaseAlert,
  BaseAvatar,
  BaseBadge,
  BaseButton,
  BaseCard,
  BaseModal,
  ErrorState,
  FormField,
  LoadingState,
  PasswordInput,
  RelativeTime,
  SelectInput,
  TextInput,
} from '@/components/base'
import InvitationStatus from '@/components/account/InvitationStatus.vue'
import NameFields from '@/components/account/NameFields.vue'
import DeliveryResult from '@/components/notifications/DeliveryResult.vue'
import { useDeliveryPoll } from '@/composables/useDeliveryPoll'
import { invalidate, useQuery } from '@/composables/useQuery'
import { useSessionStore } from '@/stores/session'
import { useWorkspaceStore } from '@/stores/workspace'
import { normUsername, USERNAME_HINT, usernameProblem, usernameTaken, userStatusLabel } from '@/usernames'
import AdminPage from './AdminPage.vue'

const route = useRoute()
const session = useSessionStore()
const workspace = useWorkspaceStore()
const userId = computed(() => String(route.params.id))
const isSelf = computed(() => userId.value === session.user?.id)

const userQ = useQuery(() => `admin:user:${userId.value}`, (signal) => adminApi.user(userId.value, { signal }))
const groupsQ = useQuery(() => 'admin:groups:all', (signal) => adminApi.allGroups({ signal }))
const userGroupsQ = useQuery(() => `admin:user-groups:${userId.value}`, (signal) => adminApi.userGroups(userId.value, { signal }))
const user = computed(() => userQ.data.value ?? null)

const notice = ref<string | null>(null)
const actionError = ref<unknown>(null)
const poll = useDeliveryPoll()

/** Other lists show this user's name, role, status or groups. */
function refreshLists() {
  // The sidebar and the ticket view read the signed-in user's own groups from the workspace store.
  if (isSelf.value) void workspace.load().catch(() => undefined)
  invalidate('admin:users')
  invalidate('admin:members')
  invalidate('admin:groups')
}

/** Text for a failed change. */
function changeError(err: unknown): unknown {
  if (!(err instanceof ApiError)) return err
  if (err.is('lastAdmin')) return 'At least one active administrator must remain, so nothing was changed.'
  if (err.is('passwordRequired')) return 'This user never set a password, so the account cannot be enabled. Set a password first.'
  if (err.is('versionConflict')) {
    void userQ.refetch()
    return 'This user was changed by someone else. The latest version is loaded; try again.'
  }
  return err
}

async function followDelivery(deliveryId: string) {
  poll.reset()
  try {
    poll.start(await deliveriesApi.get(deliveryId), (id, signal) => deliveriesApi.get(id, { signal }))
  } catch {
    // The delivery log is optional context; the invitation itself was created.
  }
}

// Arriving from Create user.
const created = route.query.created
if (created === 'invite') {
  notice.value = 'The user was created and invited. The link is valid for 72 hours.'
  adminApi
    .invitation(userId.value)
    .then((inv) => followDelivery(inv.deliveryId))
    .catch(() => {
      notice.value = 'The user was created, but the invitation email could not be queued. Use “Send invitation again”.'
    })
} else if (created === 'password') {
  notice.value = 'The user was created and can sign in with their username or email address, using the initial password.'
}

// ---- Details ----
const draft = ref<NameParts>({ givenName: '', middleName: '', familyName: '' })
const username = ref('')
const detailsBusy = ref(false)
const detailsError = ref<unknown>(null)
const dfe = (f: string): string | undefined => (detailsError.value instanceof ApiError ? detailsError.value.field(f) : undefined)
function resetDetails(u: UserSummary | null) {
  if (!u) return
  draft.value = { givenName: u.givenName, middleName: u.middleName, familyName: u.familyName }
  username.value = u.username
}
watch(user, resetDetails, { immediate: true })
const detailsChanged = computed(() => {
  const u = user.value
  if (!u) return false
  const d = draft.value
  return (
    d.givenName.trim() !== u.givenName ||
    d.middleName.trim() !== u.middleName ||
    d.familyName.trim() !== u.familyName ||
    normUsername(username.value) !== u.username
  )
})
const usernameError = computed(() => usernameProblem(username.value) ?? usernameTaken(detailsError.value) ?? dfe('username'))
const canSaveDetails = computed(
  () => detailsChanged.value && !!draft.value.givenName.trim() && !!draft.value.familyName.trim() && !!normUsername(username.value) && !usernameProblem(username.value),
)

async function saveDetails() {
  const u = user.value
  if (!u || !canSaveDetails.value) return
  detailsBusy.value = true
  detailsError.value = null
  notice.value = null
  const d = draft.value
  const patch: Partial<NameParts> & { username?: string } = {}
  if (d.givenName.trim() !== u.givenName) patch.givenName = d.givenName.trim()
  if (d.middleName.trim() !== u.middleName) patch.middleName = d.middleName.trim()
  if (d.familyName.trim() !== u.familyName) patch.familyName = d.familyName.trim()
  if (normUsername(username.value) !== u.username) patch.username = normUsername(username.value)
  try {
    const saved = await adminApi.updateUser(u, patch)
    userQ.set(saved)
    notice.value = patch.username ? `Saved. ${saved.name} now signs in with “${saved.username}” (or their email address); the old username stopped working.` : 'Saved.'
    refreshLists()
  } catch (err) {
    detailsError.value = usernameTaken(err) || (err instanceof ApiError && err.fieldErrors.length) ? err : changeError(err)
  } finally {
    detailsBusy.value = false
  }
}

// ---- Role, status and security actions (confirmed in a dialog) ----
type Kind = 'grant_admin' | 'revoke_admin' | 'disable' | 'enable' | 'reset_mfa' | 'revoke_invitation'
const copy: Record<Kind, { title: string; button: string; danger: boolean }> = {
  grant_admin: { title: 'Make administrator?', button: 'Make administrator', danger: false },
  revoke_admin: { title: 'Remove administrator role?', button: 'Remove administrator role', danger: true },
  disable: { title: 'Deactivate this user?', button: 'Deactivate user', danger: true },
  enable: { title: 'Reactivate this user?', button: 'Reactivate user', danger: false },
  reset_mfa: { title: 'Reset two-step verification?', button: 'Reset two-step verification', danger: true },
  revoke_invitation: { title: 'Revoke invitation?', button: 'Revoke invitation', danger: true },
}
const pending = ref<Kind | null>(null)
const confirmOpen = computed({
  get: () => pending.value !== null,
  set: (v) => {
    if (!v) pending.value = null
  },
})
const busy = ref(false)

function ask(kind: Kind) {
  actionError.value = null
  pending.value = kind
}

async function confirmAction() {
  const u = user.value
  const kind = pending.value
  if (!u || !kind) return
  busy.value = true
  actionError.value = null
  notice.value = null
  try {
    if (kind === 'grant_admin' || kind === 'revoke_admin') {
      userQ.set(await adminApi.updateUser(u, { isAdmin: kind === 'grant_admin' }))
      notice.value = kind === 'grant_admin' ? `${u.name} is now an administrator.` : `${u.name} is now an agent.`
    } else if (kind === 'disable' || kind === 'enable') {
      userQ.set(await adminApi.updateUser(u, { status: kind === 'disable' ? 'disabled' : 'active' }))
      notice.value =
        kind === 'disable'
          ? `${u.name} was signed out everywhere and can no longer sign in. Their open tickets go back to their groups' unassigned queues.`
          : `${u.name} can sign in again.`
    } else if (kind === 'reset_mfa') {
      await adminApi.resetUserMfa(u.id)
      notice.value = `Two-step verification was removed for ${u.name}. They sign in with their password and can set it up again.`
      void userQ.refetch()
    } else {
      try {
        await adminApi.revokeInvitation(u.id)
      } catch (err) {
        if (!(err instanceof ApiError && err.is('invitationNotFound'))) throw err
      }
      notice.value = `The invitation for ${u.email} no longer works.`
      poll.reset()
      invalidate(`admin:users:invitation:${u.id}`)
    }
    refreshLists()
  } catch (err) {
    actionError.value = changeError(err)
  } finally {
    pending.value = null
    busy.value = false
  }
}

async function sendInvitation() {
  const u = user.value
  if (!u) return
  busy.value = true
  actionError.value = null
  notice.value = null
  try {
    const inv = await adminApi.sendInvitation(u.id)
    notice.value = `Invitation sent to ${u.email}. The link works once; older links no longer work.`
    invalidate(`admin:users:invitation:${u.id}`)
    await followDelivery(inv.deliveryId)
  } catch (err) {
    actionError.value = err instanceof ApiError && err.is('userNotInvited') ? `${u.email} has already accepted or is disabled.` : err
  } finally {
    busy.value = false
  }
}

// ---- Set password ----
const pwOpen = ref(false)
const newPassword = ref('')
const pwBusy = ref(false)
const pwError = ref<unknown>(null)
const pwFe = computed(() => (pwError.value instanceof ApiError ? pwError.value.field('password') : undefined))
function openPassword() {
  newPassword.value = ''
  pwError.value = null
  pwOpen.value = true
}
async function savePassword() {
  const u = user.value
  if (!u || !newPassword.value) return
  pwBusy.value = true
  pwError.value = null
  try {
    await adminApi.setUserPassword(u.id, newPassword.value)
    pwOpen.value = false
    newPassword.value = ''
    notice.value = `The password of ${u.name} was set and their other sessions were signed out.${u.status === 'invited' ? ' The account is now active.' : ''}`
    void userQ.refetch()
    refreshLists()
  } catch (err) {
    pwError.value = err
  } finally {
    pwBusy.value = false
  }
}

// ---- Groups ----
const activeGroups = computed<Group[]>(() => (groupsQ.data.value ?? []).filter((g) => g.active))
const selected = ref<Set<string>>(new Set())
watch(
  () => userGroupsQ.data.value,
  (v) => {
    selected.value = new Set((v?.groups ?? []).map((g) => g.id))
  },
  { immediate: true },
)
const current = computed(() => new Set((userGroupsQ.data.value?.groups ?? []).map((g) => g.id)))
const removing = computed(() => activeGroups.value.filter((g) => current.value.has(g.id) && !selected.value.has(g.id)))
const groupsChanged = computed(() => selected.value.size !== current.value.size || [...selected.value].some((id) => !current.value.has(id)))
const groupsBusy = ref(false)
const groupsError = ref<unknown>(null)
const groupOptions = computed(() => activeGroups.value.map((g) => ({ value: g.id, label: g.name, hint: g.description ?? undefined })))
const selectedGroupIds = computed<string[]>({
  get: () => [...selected.value],
  set: (v) => (selected.value = new Set(v)),
})
async function saveGroups() {
  const u = user.value
  if (!u) return
  groupsBusy.value = true
  groupsError.value = null
  notice.value = null
  try {
    userGroupsQ.set(await adminApi.setUserGroups(u.id, [...selected.value]))
    notice.value = `Groups of ${u.name} saved.`
    refreshLists()
  } catch (err) {
    groupsError.value = err instanceof ApiError && err.is('groupInactive') ? 'A selected group was deactivated meanwhile. The list has been refreshed.' : err
    void groupsQ.refetch()
    void userGroupsQ.refetch()
  } finally {
    groupsBusy.value = false
  }
}
</script>

<template>
  <AdminPage :title="user?.name ?? 'User'" section="Users" :description="user ? `${user.email} · ${user.isAdmin ? 'Administrator' : 'Agent'} · ${userStatusLabel(user.status)}` : undefined">
    <template v-if="user" #avatar>
      <BaseAvatar :name="user.name" role="staff" size="md" :admin="user.isAdmin" decorative />
    </template>
    <RouterLink :to="{ name: 'admin-users' }" class="inline-flex items-center gap-1.5 self-start text-[13px] text-muted hover:text-text hover:underline">
      <IconArrowLeft :size="14" stroke="1.75" aria-hidden="true" />All users
    </RouterLink>
    <LoadingState v-if="userQ.loading.value && !user" />
    <ErrorState v-else-if="userQ.error.value && !user" :error="userQ.error.value" title="Could not load this user" @retry="userQ.refetch" />
    <template v-else-if="user">
      <BaseAlert v-if="actionError" :error="actionError" class="max-w-2xl" />
      <BaseAlert v-if="notice" tone="info" class="max-w-2xl">{{ notice }}</BaseAlert>
      <DeliveryResult v-if="poll.delivery.value" :delivery="poll.delivery.value" :polling="poll.polling.value" :gave-up="poll.gaveUp.value" class="max-w-2xl" />

      <BaseCard title="Details" class="max-w-2xl">
        <BaseAlert v-if="!user.familyName" tone="warn">This user has no last name yet (their name was stored as one word). Add it below.</BaseAlert>
        <form class="flex flex-col gap-3" novalidate @submit.prevent="saveDetails">
          <BaseAlert v-if="detailsError && !usernameTaken(detailsError) && !(detailsError instanceof ApiError && detailsError.fieldErrors.length)" :error="detailsError" />
          <NameFields v-model="draft" :errors="dfe" />
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Username" :hint="`${USERNAME_HINT} The old username stops working immediately.`" :error="usernameError" required>
              <TextInput v-model="username" autocomplete="off" autocapitalize="off" spellcheck="false" mono />
            </FormField>
            <FormField label="Email" hint="The email address cannot be changed.">
              <TextInput :model-value="user.email" type="email" readonly />
            </FormField>
          </div>
          <div class="flex gap-2">
            <BaseButton type="submit" variant="primary" :loading="detailsBusy" :disabled="!canSaveDetails">Save details</BaseButton>
            <BaseButton :disabled="!detailsChanged || detailsBusy" @click="resetDetails(user)">Discard changes</BaseButton>
          </div>
        </form>
        <dl class="m-0 grid grid-cols-[140px_1fr] gap-x-4 gap-y-1 text-[13px]">
          <dt class="text-muted">Created</dt>
          <dd class="m-0"><RelativeTime :iso="user.createdAt" /></dd>
          <dt class="text-muted">Last changed</dt>
          <dd class="m-0"><RelativeTime :iso="user.updatedAt" /></dd>
        </dl>
      </BaseCard>

      <BaseCard title="Role" class="max-w-2xl">
        <p class="m-0">
          <strong>{{ user.isAdmin ? 'Administrator' : 'Agent' }}.</strong>
          {{ user.isAdmin ? 'Manages users, groups, settings, integrations and webhooks, and can do everything an agent can.' : 'Works the tickets of their groups.' }}
        </p>
        <p v-if="isSelf" class="m-0 text-xs text-muted">You cannot change your own role.</p>
        <div v-else>
          <BaseButton v-if="user.isAdmin" @click="ask('revoke_admin')">Make agent…</BaseButton>
          <BaseButton v-else :disabled="user.status === 'disabled'" @click="ask('grant_admin')">Make administrator…</BaseButton>
        </div>
      </BaseCard>

      <BaseCard title="Status" class="max-w-2xl">
        <div class="flex flex-col gap-1">
          <p class="m-0"><BaseBadge :tone="user.status === 'active' ? 'success' : user.status === 'invited' ? 'warn' : 'neutral'">{{ userStatusLabel(user.status) }}</BaseBadge>
            <template v-if="user.status === 'active'"> Can sign in.</template>
            <template v-else-if="user.status === 'disabled'"> Cannot sign in; their messages and history stay.</template>
            <template v-else> Has not accepted the invitation yet.</template>
          </p>
          <InvitationStatus v-if="user.status === 'invited'" :user-id="user.id" />
        </div>
        <div class="flex flex-wrap gap-2">
          <template v-if="user.status === 'invited'">
            <BaseButton :loading="busy" @click="sendInvitation"><template #icon><IconSend :size="16" stroke="1.75" aria-hidden="true" /></template>Send invitation again</BaseButton>
            <BaseButton variant="danger" @click="ask('revoke_invitation')"><template #icon><IconBan :size="16" stroke="1.75" aria-hidden="true" /></template>Revoke invitation…</BaseButton>
          </template>
          <BaseButton v-if="user.status === 'active' && !isSelf" variant="danger" @click="ask('disable')"><template #icon><IconPlayerPause :size="16" stroke="1.75" aria-hidden="true" /></template>Deactivate…</BaseButton>
          <BaseButton v-if="user.status === 'disabled'" @click="ask('enable')"><template #icon><IconPlayerPlay :size="16" stroke="1.75" aria-hidden="true" /></template>Reactivate…</BaseButton>
        </div>
        <p v-if="isSelf && user.status === 'active'" class="m-0 text-xs text-muted">You cannot deactivate your own account.</p>
      </BaseCard>

      <BaseCard title="Groups" class="max-w-2xl">
        <p class="m-0 text-muted">Groups are the team queues: the user works the tickets of the groups they belong to.</p>
        <LoadingState v-if="(groupsQ.loading.value && !groupsQ.data.value) || (userGroupsQ.loading.value && !userGroupsQ.data.value)" size="sm" />
        <ErrorState v-else-if="groupsQ.error.value || userGroupsQ.error.value" :error="groupsQ.error.value ?? userGroupsQ.error.value" title="Could not load groups" @retry="() => { groupsQ.refetch(); userGroupsQ.refetch() }" />
        <form v-else class="flex flex-col gap-3" novalidate @submit.prevent="saveGroups">
          <BaseAlert v-if="groupsError" :error="groupsError" />
          <FormField :label="`Groups of ${user.name}`" hint="Search and tick the groups the user works the tickets of.">
            <SelectInput
              v-model="selectedGroupIds"
              :options="groupOptions"
              multiple
              searchable
              :max-chips="6"
              placeholder="No groups"
              search-placeholder="Find a group"
              empty-text="No active groups match"
            />
          </FormField>
          <BaseAlert v-if="removing.length" tone="warn">
            Leaving {{ removing.map((g) => g.name).join(', ') }}: tickets of {{ removing.length === 1 ? 'that group' : 'those groups' }} assigned to
            {{ user.name }} go back to the group's unassigned queue.
          </BaseAlert>
          <div><BaseButton type="submit" variant="primary" :loading="groupsBusy" :disabled="!groupsChanged">Save groups</BaseButton></div>
        </form>
      </BaseCard>

      <BaseCard title="Security" class="max-w-2xl">
        <dl class="m-0 grid grid-cols-[180px_1fr] gap-x-4 gap-y-1">
          <dt class="text-muted">Two-step verification</dt>
          <dd class="m-0">{{ user.mfaEnabled ? 'On' : 'Off' }}</dd>
        </dl>
        <div class="flex flex-wrap gap-2">
          <BaseButton @click="openPassword"><template #icon><IconKey :size="16" stroke="1.75" aria-hidden="true" /></template>Set password…</BaseButton>
          <BaseButton v-if="user.mfaEnabled && !isSelf" variant="danger" @click="ask('reset_mfa')"><template #icon><IconShieldOff :size="16" stroke="1.75" aria-hidden="true" /></template>Reset two-step verification…</BaseButton>
        </div>
        <p class="m-0 text-xs text-muted">Setting a password signs the user out of every other session. Deactivating signs them out everywhere.</p>
      </BaseCard>

      <BaseModal v-model:open="confirmOpen" :title="pending ? copy[pending].title : ''">
        <p v-if="pending === 'disable'" class="m-0">
          <strong>{{ user.name }}</strong> ({{ user.email }}) is signed out everywhere at once and can no longer sign in. Tickets
          assigned to them that are still open go back to their group's unassigned queue. Their messages and history stay. You
          can reactivate the account later.
        </p>
        <p v-else-if="pending === 'enable'" class="m-0"><strong>{{ user.name }}</strong> can sign in again with their existing password. Group memberships are unchanged.</p>
        <p v-else-if="pending === 'grant_admin'" class="m-0">
          <strong>{{ user.name }}</strong> will be able to manage users, groups, settings and integrations. If administrators must use
          two-step verification, they set it up at their next sign-in.
        </p>
        <p v-else-if="pending === 'revoke_admin'" class="m-0">
          <strong>{{ user.name }}</strong> keeps their group memberships but can no longer manage the workspace. At least one active
          administrator must remain.
        </p>
        <p v-else-if="pending === 'reset_mfa'" class="m-0">
          This removes the authenticator and recovery codes of <strong>{{ user.name }}</strong>. They can then sign in with their
          password alone. Only do this after confirming their identity, for example when they lost their device.
        </p>
        <p v-else-if="pending" class="m-0">The invitation link sent to <strong>{{ user.email }}</strong> stops working. You can send a new invitation later.</p>
        <template #footer>
          <BaseButton @click="pending = null">Cancel</BaseButton>
          <BaseButton :variant="pending && !copy[pending].danger ? 'primary' : 'danger'" :loading="busy" @click="confirmAction">{{ pending ? copy[pending].button : '' }}</BaseButton>
        </template>
      </BaseModal>

      <BaseModal v-model:open="pwOpen" :title="`Set password for ${user.name}`">
        <form id="set-password-form" class="flex flex-col gap-3" novalidate @submit.prevent="savePassword">
          <BaseAlert v-if="pwError && !pwFe" :error="pwError" />
          <FormField label="New password" hint="12 to 256 characters. Share it securely; the user can change it in their account under Security." :error="pwFe" required>
            <PasswordInput v-model="newPassword" autocomplete="new-password" />
          </FormField>
        </form>
        <template #footer>
          <BaseButton @click="pwOpen = false">Cancel</BaseButton>
          <BaseButton type="submit" form="set-password-form" variant="primary" :loading="pwBusy" :disabled="!newPassword">Set password</BaseButton>
        </template>
      </BaseModal>
    </template>
  </AdminPage>
</template>
