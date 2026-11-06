<script setup lang="ts">
import EntityIcon from '@/components/base/EntityIcon.vue'
import { IconBan, IconChevronDown, IconCopy, IconPencil, IconPlus, IconRefresh } from '@tabler/icons-vue'
import { computed, ref, watch } from 'vue'
import { adminApi, settingsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Integration, IntegrationCreated } from '@/api/types'
import {
  BaseAlert,
  BaseAvatar,
  BaseBadge,
  BaseButton,
  BaseModal,
  CheckboxInput,
  CodeBlock,
  ErrorState,
  FormField,
  LoadingState,
  RelativeTime,
  TextInput,
} from '@/components/base'
import { queryScopeSignal, useQuery } from '@/composables/useQuery'
import AdminPage from './AdminPage.vue'

const SETUP_GUIDE = 'https://github.com/arksnorman/open-kase#connect-an-external-ai-agent-mcp'

// ---- Connect card
const settings = useQuery(
  () => 'settings:integrations-connect',
  (signal) => settingsApi.get({ signal }),
)
const baseUrl = computed(() => (settings.data.value?.product.baseUrl || window.location.origin).replace(/\/+$/, ''))
const connectRows = computed(() => [
  { key: 'rest', label: 'REST API', value: `${baseUrl.value}/api/v1` },
  { key: 'mcp', label: 'MCP (AI agents)', value: `${baseUrl.value}/mcp` },
  { key: 'header', label: 'Send the token as', value: 'Authorization: Bearer kase_live_…' },
])
const copiedKey = ref<string | null>(null)
let copiedTimer: ReturnType<typeof setTimeout> | undefined
/** Clipboard API where available (secure contexts), else a hidden textarea and execCommand. Returns whether it really copied. */
async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // Inside an open modal <dialog> everything outside it is inert: the textarea must live in the dialog.
    const active = document.activeElement as HTMLElement | null
    const host = active?.closest('dialog') ?? document.body
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0'
    host.appendChild(ta)
    ta.focus()
    ta.select()
    let ok = false
    try {
      ok = document.execCommand('copy')
    } catch {
      ok = false
    }
    ta.remove()
    active?.focus?.()
    return ok
  }
}
const copyFailed = ref(false)
async function copyRow(key: string, text: string) {
  copyFailed.value = false
  if (await copyText(text)) {
    copiedKey.value = key
    clearTimeout(copiedTimer)
    copiedTimer = setTimeout(() => (copiedKey.value = null), 2000)
  } else {
    copiedKey.value = null
    copyFailed.value = true
  }
}

// ---- List (first page from the query, "Load more" appends; revoked at the end)
const list = useQuery(
  () => 'admin:integrations',
  (signal) => adminApi.integrations({ signal }),
)
/** Every loaded row; create, rename and revoke change it in place so loaded pages stay. */
const items = ref<Integration[]>([])
const cursor = ref<string | null>(null)
const loadingMore = ref(false)
const moreError = ref<unknown>(null)
watch(
  () => list.data.value,
  (d) => {
    items.value = d?.items ? [...d.items] : []
    cursor.value = d?.nextCursor ?? null
  },
  { immediate: true, flush: 'sync' },
)
async function loadMore() {
  if (!cursor.value) return
  loadingMore.value = true
  moreError.value = null
  try {
    const page = await adminApi.integrations({ signal: queryScopeSignal() }, cursor.value)
    const have = new Set(items.value.map((i) => i.id))
    items.value = [...items.value, ...page.items.filter((i) => !have.has(i.id))]
    cursor.value = page.nextCursor
  } catch (err) {
    moreError.value = err
  } finally {
    loadingMore.value = false
  }
}
const rows = computed(() => [...items.value.filter((i) => i.status === 'active'), ...items.value.filter((i) => i.status !== 'active')])

// ---- Scopes: plain-language names, grouped
const GROUPS = [
  { label: 'Tickets', prefix: 'tickets:' },
  { label: 'Contacts', prefix: 'contacts:' },
  { label: 'Tags', prefix: 'tags:' },
  { label: 'Hold reasons', prefix: 'holdReasons:' },
  { label: 'Organisations', prefix: 'organisations:' },
]
/** Short chip text per scope; unknown scopes fall back to the part after the colon. */
const CHIP: Record<string, string> = {
  'tickets:read': 'read',
  'tickets:create': 'create',
  'tickets:replyAsRequester': 'reply as requester, resolve',
  'tickets:comment': 'reply as staff',
  'tickets:internal': 'internal notes',
  'tickets:write': 'change & assign',
  'tickets:all': 'all ticket access',
  'contacts:read': 'read',
  'contacts:write': 'create & edit',
  'tags:read': 'read',
  'tags:write': 'add & delete',
  'holdReasons:read': 'read',
  'holdReasons:write': 'create, edit & delete',
  'organisations:read': 'read',
  'organisations:write': 'create, edit & delete',
}
const TITLE: Record<string, string> = {
  'tickets:read': 'Read tickets and public messages',
  'tickets:create': 'Create tickets',
  'tickets:replyAsRequester': 'Reply as the requester and resolve tickets',
  'tickets:comment': 'Reply as staff',
  'tickets:internal': 'Read and write internal notes',
  'tickets:write': 'Change status, priority, tags; transfer and assign',
  'tickets:all': 'All ticket permissions',
  'contacts:read': 'Read contacts',
  'contacts:write': 'Create and edit contacts',
  'tags:read': 'Read the tag list',
  'tags:write': 'Add and delete tags',
  'holdReasons:read': 'Read the hold reasons',
  'holdReasons:write': 'Create, change and delete hold reasons',
  'organisations:read': 'Read the organisations',
  'organisations:write': 'Create, change and delete organisations',
}
const chip = (s: string) => CHIP[s] ?? s.split(':')[1] ?? s
function grouped(scopes: string[]) {
  return GROUPS.map((g) => ({ label: g.label, items: scopes.filter((s) => s.startsWith(g.prefix)) })).filter((g) => g.items.length)
}
const STAFF_SCOPES = ['tickets:comment', 'tickets:internal', 'tickets:write', 'tickets:all']
/** Same rule as the API (access.StaffScopes): any staff scope makes the token act as staff. */
const isStaff = (scopes: string[]) => scopes.some((s) => STAFF_SCOPES.includes(s))

// ---- Create (dialog, step 1) then token (step 2)
const createOpen = ref(false)
const name = ref('')
const scopeList = useQuery(() => 'integration-scopes', (signal) => adminApi.integrationScopes({ signal }))
const allScopes = computed(() => scopeList.data.value?.items ?? [])
const scopeGroups = computed(() =>
  GROUPS.map((g) => ({ label: g.label, items: allScopes.value.filter((s) => s.name.startsWith(g.prefix)) })).filter((g) => g.items.length),
)
const presets = [
  { id: 'agent', title: 'AI agent / staff bot', text: 'Reads tickets, replies as staff, notes, updates', scopes: ['tickets:read', 'tickets:comment', 'tickets:internal', 'tickets:write', 'tags:read'] },
  { id: 'portal', title: 'Requester portal', text: 'Creates tickets, replies for and resolves as the requester', scopes: ['tickets:read', 'tickets:create', 'tickets:replyAsRequester', 'contacts:read', 'contacts:write', 'organisations:read', 'organisations:write'] },
  { id: 'readonly', title: 'Read only', text: 'Reads tickets, for reports or dashboards', scopes: ['tickets:read'] },
  { id: 'custom', title: 'Custom', text: 'Pick every permission yourself', scopes: [] as string[] },
]
const preset = ref<string>('custom')
const scopes = ref<string[]>([])
function setScopes(next: string[]) {
  const all = allScopes.value.map((s) => s.name)
  scopes.value = all.filter((s) => next.includes(s)) // the server's order
}
function pickPreset(id: string) {
  preset.value = id
  setScopes(presets.find((p) => p.id === id)?.scopes ?? [])
}
function toggleScope(scope: string, on: boolean) {
  setScopes(on ? [...scopes.value, scope] : scopes.value.filter((s) => s !== scope))
  preset.value = 'custom'
}
const actsAsStaff = computed(() => allScopes.value.some((s) => s.staff && scopes.value.includes(s.name)))

const saving = ref(false)
const error = ref<unknown>(null)
const fe = (f: string) => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const created = ref<IntegrationCreated | null>(null)
const copiedToken = ref(false)

function openCreate() {
  name.value = ''
  scopes.value = []
  preset.value = 'custom'
  error.value = null
  createOpen.value = true
}

async function create() {
  saving.value = true
  error.value = null
  try {
    const result = await adminApi.createIntegration({ name: name.value.trim(), scopes: scopes.value })
    createOpen.value = false
    created.value = result
    copiedToken.value = false
    name.value = ''
    scopes.value = []
    const { token: _token, ...row } = result
    items.value = [row, ...items.value.filter((i) => i.id !== row.id)]
  } catch (err) {
    error.value = err
  } finally {
    saving.value = false
  }
}
async function copyToken() {
  if (!created.value) return
  copyFailed.value = false
  copiedToken.value = await copyText(created.value.token)
  copyFailed.value = !copiedToken.value
}
const tokenOpen = computed({
  get: () => created.value !== null,
  // Dropping the reference is the only copy the UI ever held.
  set: (v: boolean) => {
    if (!v) created.value = null
  },
})

// ---- Rename (scopes stay as issued; revoked credentials are read-only)
const renaming = ref<Integration | null>(null)
const renameText = ref('')
const renameBusy = ref(false)
const renameError = ref<unknown>(null)
const renameOpen = computed({
  get: () => renaming.value !== null,
  set: (v: boolean) => {
    if (!v) renaming.value = null
  },
})
function openRename(i: Integration) {
  renaming.value = i
  renameText.value = i.name
  renameError.value = null
}
async function rename() {
  const target = renaming.value
  if (!target) return
  renameBusy.value = true
  renameError.value = null
  try {
    const updated = await adminApi.renameIntegration(target, renameText.value.trim())
    items.value = items.value.map((i) => (i.id === updated.id ? updated : i))
    renaming.value = null
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      await list.refetch()
      renaming.value = items.value.find((i) => i.id === target.id) ?? target
      renameError.value = new Error('Someone else changed this integration. The latest version is loaded; save again to apply your name.')
    } else renameError.value = err
  } finally {
    renameBusy.value = false
  }
}

// ---- Revoke
const revoking = ref<Integration | null>(null)
const revokeBusy = ref(false)
const revokeError = ref<unknown>(null)
const revokeOpen = computed({
  get: () => revoking.value !== null,
  set: (v: boolean) => {
    if (!v) revoking.value = null
  },
})
async function revoke() {
  const target = revoking.value
  if (!target) return
  revokeBusy.value = true
  revokeError.value = null
  try {
    await adminApi.deleteIntegration(target.id)
    const at = new Date().toISOString()
    items.value = items.value.map((i) => (i.id === target.id ? { ...i, status: 'revoked', revokedAt: at } : i))
    revoking.value = null
  } catch (err) {
    revokeError.value = err
  } finally {
    revokeBusy.value = false
  }
}
</script>

<template>
  <AdminPage title="Integrations" description="Tokens for external systems and AI agents. Each token can only do what you allow. They work with the REST API and MCP.">
    <template #actions>
      <BaseButton variant="primary" aria-haspopup="dialog" @click="openCreate">
        <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
        New integration
      </BaseButton>
    </template>

    <!-- Connect -->
    <section class="lg-panel flex flex-col gap-2.5 p-4" aria-labelledby="connect-h">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="connect-h" class="lg-h3">Connect</h2>
        <a :href="SETUP_GUIDE" target="_blank" rel="noopener noreferrer" class="text-xs text-accent-text">Setup guide</a>
      </div>
      <dl class="m-0 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3.5 gap-y-2 max-sm:grid-cols-1">
        <template v-for="r in connectRows" :key="r.key">
          <dt class="text-muted">{{ r.label }}</dt>
          <dd class="m-0 min-w-0"><code class="lg-mono block break-all rounded-md bg-subtle px-2 py-1 text-xs">{{ r.value }}</code></dd>
          <dd class="m-0">
            <BaseButton size="sm" :aria-label="`Copy ${r.label}`" @click="copyRow(r.key, r.value)">
              <template #icon><IconCopy :size="14" stroke="1.75" aria-hidden="true" /></template>
              {{ copiedKey === r.key ? 'Copied' : 'Copy' }}
            </BaseButton>
          </dd>
        </template>
      </dl>
      <span class="sr-only" role="status">{{ copiedKey ? 'Copied to clipboard' : '' }}</span>
    <p v-if="copyFailed" role="alert" class="lg-field-error m-0">Copying failed. Select the text and copy it manually.</p>
    </section>

    <!-- Tokens -->
    <LoadingState v-if="list.loading.value && !list.data.value" text="Loading integrations…" />
    <ErrorState v-else-if="list.error.value" :error="list.error.value" @retry="list.refetch" />
    <div v-else-if="!rows.length" class="lg-panel flex flex-col items-center gap-2 px-4 py-8 text-center text-muted">
      <EntityIcon kind="bot" size="lg" />
      <p class="m-0">No integrations yet.</p>
    </div>
    <ul v-else class="lg-panel m-0 flex list-none flex-col p-0" aria-label="Integrations">
      <li
        v-for="(i, n) in rows"
        :key="i.id"
        class="grid grid-cols-[minmax(0,1.2fr)_minmax(0,2fr)_auto] gap-4 px-4 py-3.5 max-md:grid-cols-1"
        :class="[n ? 'border-0 border-t border-solid border-border' : '', i.status === 'revoked' ? 'opacity-60' : '']"
      >
        <div class="flex min-w-0 items-start gap-3">
         <BaseAvatar :name="i.name" role="bot" size="md" decorative class="mt-0.5" />
         <div class="flex min-w-0 flex-1 flex-col gap-1">
          <b class="lg-ell text-[15px]">{{ i.name }}</b>
          <div class="flex flex-wrap gap-1.5">
            <BaseBadge v-if="i.status === 'active'" tone="success">Active</BaseBadge>
            <BaseBadge v-else>Revoked<template v-if="i.revokedAt">&nbsp;<RelativeTime :iso="i.revokedAt" /></template></BaseBadge>
            <span v-if="isStaff(i.scopes)" class="lg-tag !border-transparent !bg-[var(--kase-staff-bg)] !font-semibold !text-[var(--kase-staff)]">
              Acts as staff
            </span>
            <span v-else class="lg-tag !border-transparent !bg-[var(--kase-req-bg)] !font-semibold !text-[var(--kase-req)]">
              Acts for requesters
            </span>
          </div>
          <span class="text-xs text-muted">
            <template v-if="i.lastUsedAt">Last used <RelativeTime :iso="i.lastUsedAt" /></template><template v-else>Never used</template>
            <template v-if="i.createdAt"> · created <RelativeTime :iso="i.createdAt" /></template>
            · <span class="lg-mono">{{ i.tokenHint }}…</span>
          </span>
         </div>
        </div>
        <div class="flex min-w-0 flex-col gap-1.5">
          <div v-for="g in grouped(i.scopes)" :key="g.label" class="flex flex-wrap items-baseline gap-1.5">
            <span class="w-16 flex-none text-xs text-muted">{{ g.label }}</span>
            <span v-for="s in g.items" :key="s" class="rounded-md border border-solid border-border bg-subtle px-1.5 text-xs" v-tooltip="s">{{ chip(s) }}</span>
          </div>
          <span v-if="!i.scopes.length" class="text-xs text-muted">No permissions</span>
        </div>
        <div class="flex items-start gap-1.5">
          <template v-if="i.status === 'active'">
            <BaseButton size="sm" :aria-label="`Rename ${i.name}`" @click="openRename(i)"><template #icon><IconPencil :size="14" stroke="1.75" aria-hidden="true" /></template>Rename</BaseButton>
            <BaseButton size="sm" variant="danger" :aria-label="`Revoke ${i.name}`" @click="revoking = i"><template #icon><IconBan :size="14" stroke="1.75" aria-hidden="true" /></template>Revoke</BaseButton>
          </template>
        </div>
      </li>
      <li v-if="cursor || moreError" class="flex flex-col items-start gap-2 border-0 border-t border-solid border-border px-4 py-2.5">
        <BaseAlert v-if="moreError" :error="moreError" />
        <BaseButton size="sm" :loading="loadingMore" @click="loadMore"><template #icon><IconChevronDown :size="14" stroke="1.75" aria-hidden="true" /></template>Load more</BaseButton>
      </li>
    </ul>

    <!-- New integration, step 1 -->
    <BaseModal v-model:open="createOpen" title="New integration" description="Step 1 of 2: name it and choose what it may do." width="lg">
      <form class="flex flex-col gap-4" novalidate @submit.prevent="create">
        <BaseAlert v-if="error && !(error instanceof ApiError && error.fieldErrors.length)" :error="error" />
        <FormField label="Name" hint="The system using it, or the bot name requesters will see on replies, e.g. “Support bot”." :error="fe('name')" required>
          <TextInput v-model="name" autocomplete="off" />
        </FormField>

        <fieldset class="m-0 flex flex-col gap-1.5 border-0 p-0">
          <legend class="lg-label mb-1 p-0">Start from</legend>
          <div class="grid grid-cols-2 gap-2 max-sm:grid-cols-1">
            <button
              v-for="p in presets"
              :key="p.id"
              type="button"
              class="lg-hov flex cursor-pointer flex-col items-start gap-0.5 rounded-[10px] border border-solid bg-transparent px-3 py-2.5 text-left text-xs text-muted"
              :class="preset === p.id ? 'border-accent bg-accent-soft shadow-[inset_0_0_0_1px_var(--kase-accent)]' : 'border-border'"
              :aria-pressed="preset === p.id"
              @click="pickPreset(p.id)"
            >
              <b class="text-[13px] text-text">{{ p.title }}</b>{{ p.text }}
            </button>
          </div>
        </fieldset>

        <fieldset class="m-0 flex flex-col gap-2 border-0 p-0" aria-describedby="integration-scopes-hint">
          <legend class="lg-label mb-0.5 p-0">Permissions</legend>
          <span id="integration-scopes-hint" class="text-xs text-muted">What the token may do; it never exceeds them. “Read tickets” is needed to work on existing tickets. Permissions cannot be changed later: create a new token and revoke the old one.</span>
          <div v-if="scopeList.error.value" class="flex flex-col items-start gap-2">
            <BaseAlert :error="scopeList.error.value" />
            <BaseButton size="sm" @click="scopeList.refetch()"><template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>Try again</BaseButton>
          </div>
          <div v-for="g in scopeGroups" :key="g.label" class="flex flex-col gap-2" role="group" :aria-label="g.label">
            <span class="mt-1 text-xs font-semibold uppercase tracking-wider text-muted">{{ g.label }}</span>
            <CheckboxInput
              v-for="s in g.items"
              :id="`scope-${s.name}`"
              :key="s.name"
              :label="TITLE[s.name] ?? s.name"
              :model-value="scopes.includes(s.name)"
              @update:model-value="toggleScope(s.name, $event)"
            >
              <template #hint>
                <code class="lg-mono text-[11px]">{{ s.name }}</code>
                <span v-if="s.description" class="block">{{ s.description }}</span>
              </template>
            </CheckboxInput>
          </div>
          <span v-if="scopes.length" class="rounded-md px-3 py-2 text-xs" :class="actsAsStaff ? 'bg-[var(--kase-staff-bg)] text-[var(--kase-staff)]' : 'bg-[var(--kase-req-bg)] text-[var(--kase-req)]'" role="status">
            <template v-if="actsAsStaff">This token will act as <b>staff</b>: it replies under its name (shown as a bot) and sees the staff ticket view.</template>
            <template v-else>This token will act <b>for requesters</b> and sees public content only.</template>
          </span>
          <span v-if="fe('scopes')" class="lg-field-error">{{ fe('scopes') }}</span>
        </fieldset>
        <div class="flex justify-end gap-2">
          <BaseButton @click="createOpen = false">Cancel</BaseButton>
          <BaseButton type="submit" variant="primary" :loading="saving" :disabled="!name.trim() || !scopes.length">Create token</BaseButton>
        </div>
      </form>
    </BaseModal>

    <!-- New integration, step 2 -->
    <BaseModal v-model:open="tokenOpen" width="md" title="Copy your token" description="Step 2 of 2: this is the only time it is shown. Store it in the system that uses it.">
      <div class="flex flex-col gap-3">
        <CodeBlock select-all>{{ created?.token }}</CodeBlock>
        <span v-if="copiedToken" role="status" class="text-xs text-muted">Copied to clipboard.</span>
        <span v-if="copyFailed" role="alert" class="lg-field-error">Copying failed. Select the token above and copy it manually.</span>
        <BaseAlert tone="warn">If it is lost, revoke this integration and create a new one.</BaseAlert>
        <div class="flex flex-col gap-0.5">
          <span class="lg-label">Next step</span>
          <span class="text-xs text-muted">
            Point your system at the REST API or MCP address from the Connect card and send this token as
            <code class="lg-mono">Authorization: Bearer …</code>.
          </span>
        </div>
      </div>
      <template #footer>
        <BaseButton @click="copyToken">
          <template #icon><IconCopy :size="16" stroke="1.75" aria-hidden="true" /></template>
          Copy token
        </BaseButton>
        <BaseButton variant="primary" @click="tokenOpen = false">Done</BaseButton>
      </template>
    </BaseModal>

    <BaseModal v-model:open="renameOpen" :title="`Rename ${renaming?.name ?? 'integration'}`" description="Only the name changes; permissions stay as issued.">
      <form id="rename-form" class="flex flex-col gap-3" novalidate @submit.prevent="rename">
        <BaseAlert v-if="renameError && !(renameError instanceof ApiError && renameError.field('name'))" :error="renameError" />
        <FormField label="Name" :error="renameError instanceof ApiError ? renameError.field('name') : undefined" required>
          <TextInput v-model="renameText" autocomplete="off" />
        </FormField>
      </form>
      <template #footer>
        <BaseButton @click="renaming = null">Cancel</BaseButton>
        <BaseButton type="submit" form="rename-form" variant="primary" :loading="renameBusy" :disabled="!renameText.trim() || renameText.trim() === renaming?.name">Save</BaseButton>
      </template>
    </BaseModal>

    <BaseModal v-model:open="revokeOpen" :title="`Revoke ${revoking?.name ?? 'integration'}?`" description="Requests using this token will be rejected immediately.">
      <BaseAlert v-if="revokeError" :error="revokeError" />
      <template #footer>
        <BaseButton @click="revoking = null">Cancel</BaseButton>
        <BaseButton variant="danger" :loading="revokeBusy" @click="revoke"><template #icon><IconBan :size="16" stroke="1.75" aria-hidden="true" /></template>Revoke token</BaseButton>
      </template>
    </BaseModal>
  </AdminPage>
</template>
