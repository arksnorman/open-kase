<script setup lang="ts">
// Configuration (admins): four sections chosen from the left list (one shown at a time), each with its own Save.
// PUT /settings takes the full document, so each section saves the last loaded document with only
// its own fields replaced; unknown sections and fields are sent back unchanged. If-Match = configVersion.
// The requester notice ("Confirm new tickets") lives on the project record and saves with Email.
import { IconPencil, IconSend, IconTrash } from '@tabler/icons-vue'
import { computed, nextTick, onMounted, ref, shallowRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi, deliveriesApi, settingsApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Settings, SmtpTlsMode } from '@/api/types'
import {
  BaseAlert,
  BaseBadge,
  BaseButton,
  BaseModal,
  FormField,
  LoadingState,
  PasswordInput,
  RelativeTime,
  SelectInput,
  SwitchInput,
  TextArea,
  TextInput,
  type SelectOption,
} from '@/components/base'
import DeliveryResult from '@/components/notifications/DeliveryResult.vue'
import { useDeliveryPoll } from '@/composables/useDeliveryPoll'
import { useQuery } from '@/composables/useQuery'
import { useSessionStore } from '@/stores/session'
import { useWorkspaceStore } from '@/stores/workspace'
import AutomationSettings from '@/components/project/AutomationSettings.vue'
import AdminPage from './AdminPage.vue'

type Section = 'general' | 'auth' | 'email'

const route = useRoute()
const router = useRouter()
// Section list (left rail, a tab row on narrow screens): shows one section at a time; the choice
// is kept in ?section= so links and reloads open the same section.
const sections = [
  { id: 'general', label: 'General' },
  { id: 'email', label: 'Email' },
  { id: 'rules', label: 'Ticket rules' },
  { id: 'security', label: 'Security' },
] as const
type SectionId = (typeof sections)[number]['id']
const activeSection = computed<SectionId>(() => {
  const q = route.query.section
  return sections.some((x) => x.id === q) ? (q as SectionId) : 'general'
})
const rulesOn = ref(0)
function goTo(id: SectionId) {
  void router.replace({ query: { ...route.query, section: id === 'general' ? undefined : id } })
  void nextTick(() => document.getElementById(id)?.focus({ preventScroll: true }))
}

const server = shallowRef<Settings | null>(null)
const loadError = ref<unknown>(null)
const loading = ref(false)

// Drafts
const general = ref({ name: '', description: '', signature: '', baseUrl: '', requesterLink: '' })
const smtp = ref({ enabled: false, host: '', port: '587', username: '', fromAddress: '', fromName: '', tlsMode: 'starttls' as SmtpTlsMode, caCertificate: '', tlsSkipVerify: false })
const passwordMode = ref<'keep' | 'replace' | 'clear'>('keep')
const password = ref('')
const requireAdminMfa = ref(false)
const session = useSessionStore()

const project = useQuery(
  () => 'project-settings',
  (signal) => adminApi.project({ signal }),
)
const ack = ref(true)
watch(
  () => project.data.value,
  (p) => {
    if (p) ack.value = p.requesterAcknowledgement
  },
  { immediate: true, flush: 'sync' },
)
const ackDirty = computed(() => !!project.data.value && ack.value !== project.data.value.requesterAcknowledgement)

function resetGeneral(s: Settings) {
  general.value = {
    name: s.product.name,
    description: s.product.description,
    signature: s.product.signature,
    baseUrl: s.product.baseUrl,
    requesterLink: s.notifications.requesterLinkBaseUrl,
  }
}
function resetSmtp(s: Settings) {
  const m = s.smtp
  smtp.value = { enabled: m.enabled, host: m.host, port: String(m.port), username: m.username, fromAddress: m.fromAddress, fromName: m.fromName, tlsMode: m.tlsMode, caCertificate: m.caCertificate, tlsSkipVerify: m.tlsSkipVerify }
  passwordMode.value = 'keep'
  password.value = ''
}
function resetAuth(s: Settings) {
  requireAdminMfa.value = s.auth.requireAdminMfa
}

async function load(keepDrafts = false) {
  loading.value = true
  loadError.value = null
  try {
    const s = await settingsApi.get()
    server.value = s
    if (!keepDrafts) {
      resetGeneral(s)
      resetSmtp(s)
      resetAuth(s)
    }
  } catch (err) {
    loadError.value = err
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())

const generalDirty = computed(() => {
  const s = server.value
  if (!s) return false
  const g = general.value
  return (
    g.name !== s.product.name || g.description.trim() !== s.product.description || g.signature.trim() !== s.product.signature || g.baseUrl !== s.product.baseUrl ||
    g.requesterLink.trim() !== s.notifications.requesterLinkBaseUrl
  )
})
const smtpDirty = computed(() => {
  const s = server.value?.smtp
  if (!s) return false
  const d = smtp.value
  return (
    d.enabled !== s.enabled || d.host !== s.host || d.port !== String(s.port) || d.username !== s.username ||
    d.fromAddress !== s.fromAddress || d.fromName !== s.fromName || d.tlsMode !== s.tlsMode ||
    d.caCertificate.trim() !== s.caCertificate || d.tlsSkipVerify !== s.tlsSkipVerify ||
    passwordMode.value === 'clear' || (passwordMode.value === 'replace' && password.value !== '')
  )
})
// Mirrors the server rule: a stored password must be sent again when it would go somewhere else or
// over a weaker check (host, port, username, security, a new CA, or skipping verification).
const passwordRequired = computed(() => {
  const s = server.value?.smtp
  if (!s || !s.passwordConfigured) return false
  const d = smtp.value
  const ca = d.caCertificate.trim()
  return (
    d.host.trim().toLowerCase() !== s.host.toLowerCase() || d.port !== String(s.port) || d.username.trim() !== s.username ||
    d.tlsMode !== s.tlsMode || (d.tlsSkipVerify && !s.tlsSkipVerify) || (ca !== '' && ca !== s.caCertificate)
  )
})
const emailDirty = computed(() => smtpDirty.value || ackDirty.value)
const requesterExample = computed(() => {
  const base = general.value.requesterLink.trim().replace(/\/+$/, '')
  return base ? `${base}/1042` : ''
})
const authDirty = computed(() => !!server.value && requireAdminMfa.value !== server.value.auth.requireAdminMfa)

// Save
const saving = ref<Section | null>(null)
const errors = ref<Record<Section, unknown>>({ general: null, auth: null, email: null })
const saved = ref<Section | null>(null)
const conflict = ref<Section | null>(null)

function fe(section: Section, field: string): string | undefined {
  const e = errors.value[section]
  return e instanceof ApiError ? e.field(field) : undefined
}
function hasOnlyFieldErrors(section: Section): boolean {
  const e = errors.value[section]
  return e instanceof ApiError && e.fieldErrors.length > 0
}

function documentWith(apply: (doc: Record<string, unknown>) => void): Record<string, unknown> {
  const doc = JSON.parse(JSON.stringify(server.value)) as Record<string, unknown>
  delete doc.configVersion
  delete doc.updatedAt
  const m = doc.smtp as Record<string, unknown> | undefined
  if (m) delete m.passwordConfigured
  apply(doc)
  return doc
}

/** The requester notice is a project field with its own version (If-Match). */
async function saveAck() {
  const p = project.data.value
  if (!p) return
  try {
    project.set(await adminApi.updateProject(p, { requesterAcknowledgement: ack.value }))
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      const keep = ack.value
      await project.refetch()
      ack.value = keep
      throw new Error('“Confirm new tickets” was changed by someone else. The latest version is loaded and your choice is kept; save again to apply it.')
    }
    throw err
  }
}

async function save(section: Section) {
  const current = server.value
  if (!current) return
  saving.value = section
  saved.value = null
  conflict.value = null
  errors.value[section] = null
  try {
    if (section === 'email' && !smtpDirty.value) {
      await saveAck()
      saved.value = 'email'
      return
    }
    const body = documentWith((doc) => {
      if (section === 'general') {
        const g = general.value
        doc.product = { ...(doc.product as object), name: g.name.trim(), description: g.description.trim(), signature: g.signature.trim(), baseUrl: g.baseUrl.trim() }
        doc.notifications = { ...(doc.notifications as object), requesterLinkBaseUrl: g.requesterLink.trim() }
      } else if (section === 'email') {
        const d = smtp.value
        const port = Number(d.port)
        const next: Record<string, unknown> = {
          ...(doc.smtp as object),
          enabled: d.enabled,
          host: d.host.trim(),
          port: Number.isFinite(port) ? port : 0,
          username: d.username.trim(),
          fromAddress: d.fromAddress.trim(),
          fromName: d.fromName.trim(),
          tlsMode: d.tlsMode,
          caCertificate: d.tlsMode === 'none' ? '' : d.caCertificate.trim(),
          tlsSkipVerify: d.tlsMode === 'none' ? false : d.tlsSkipVerify,
        }
        if (passwordMode.value === 'clear') next.password = ''
        else if (passwordMode.value === 'replace' && password.value !== '') next.password = password.value
        doc.smtp = next
      } else {
        doc.auth = { ...(doc.auth as object), requireAdminMfa: requireAdminMfa.value }
      }
    })
    const next = await settingsApi.put(body, current.configVersion)
    server.value = next
    if (section === 'general') {
      resetGeneral(next)
      // The app header shows the product name (GET /project).
      void useWorkspaceStore().load().catch(() => undefined)
    } else if (section === 'email') {
      resetSmtp(next)
      if (ackDirty.value) await saveAck()
    } else resetAuth(next)
    saved.value = section
    // The policy applies to this session at once: enrol before anything else works.
    if (section === 'auth' && next.auth.requireAdminMfa && session.user && !session.user.mfaEnabled) {
      session.mfaEnrolmentRequired = true
    }
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) conflict.value = section
    else errors.value[section] = err
  } finally {
    saving.value = null
  }
}

async function reloadKeeping() {
  conflict.value = null
  await load(true)
}
async function discard() {
  conflict.value = null
  await load(false)
}

const tlsOptions: SelectOption<SmtpTlsMode>[] = [
  { value: 'starttls', label: 'STARTTLS (required), usually port 587' },
  { value: 'tls', label: 'Implicit TLS, usually port 465' },
  { value: 'none', label: 'None, local relay only, no username' },
]

// Test email (dialog)
const testOpen = ref(false)
const testTo = ref('')
const testBusy = ref(false)
const testError = ref<unknown>(null)
const poll = useDeliveryPoll()
watch(testOpen, (o) => {
  if (!o) poll.reset()
})
function openTest() {
  testError.value = null
  poll.reset()
  testOpen.value = true
}
async function sendTest() {
  testBusy.value = true
  testError.value = null
  poll.reset()
  try {
    const res = await settingsApi.testEmail(testTo.value.trim())
    poll.start(res.delivery, (id, signal) => deliveriesApi.get(id, { signal }))
  } catch (err) {
    testError.value = err
  } finally {
    testBusy.value = false
  }
}
</script>

<template>
  <AdminPage title="Configuration" description="Your installation’s name, email, ticket rules and security. Secrets are write-only; changes apply to running servers within a few seconds.">
    <BaseAlert v-if="loadError" :error="loadError" />
    <LoadingState v-if="loading && !server" text="Loading configuration…" />

    <div v-if="server" class="grid grid-cols-[190px_minmax(0,1fr)] items-start gap-6 max-md:grid-cols-1">
      <nav aria-label="Sections" class="sticky top-3 z-10 flex flex-col gap-0.5 max-md:static max-md:flex-row max-md:overflow-x-auto max-md:border-0 max-md:border-b max-md:border-solid max-md:border-border max-md:pb-2">
        <a
          v-for="sec in sections"
          :key="sec.id"
          :href="`#${sec.id}`"
          class="lg-hov flex flex-none items-center justify-between gap-2 rounded-md px-2.5 py-1.5 text-muted no-underline"
          :class="activeSection === sec.id ? '!bg-surface font-semibold !text-text shadow-[inset_0_0_0_1px_var(--kase-border)]' : ''"
          :aria-current="activeSection === sec.id ? 'true' : undefined"
          @click.prevent="goTo(sec.id)"
        >
          {{ sec.label }}
          <BaseBadge v-if="sec.id === 'email'" :tone="server.smtp.enabled ? 'success' : 'neutral'">{{ server.smtp.enabled ? 'On' : 'Off' }}</BaseBadge>
          <BaseBadge v-else-if="sec.id === 'rules'" :tone="rulesOn ? 'success' : 'neutral'">{{ rulesOn ? `${rulesOn} on` : 'Off' }}</BaseBadge>
        </a>
      </nav>

      <div class="flex min-w-0 max-w-[720px] flex-col gap-4">

        <!-- General -->
        <section v-show="activeSection === 'general'" id="general" tabindex="-1" aria-labelledby="general-h" class="lg-panel flex scroll-mt-4 flex-col gap-4 p-4 outline-none">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 id="general-h" class="lg-h3">General</h2>
              <p class="m-0 mt-0.5 text-muted">How this installation is named and where it lives.</p>
            </div>
            <span class="text-xs text-muted">Updated <RelativeTime :iso="server.updatedAt" /></span>
          </div>
          <form class="flex flex-col gap-3" novalidate @submit.prevent="save('general')">
            <BaseAlert v-if="conflict === 'general'" tone="warn">
              <div class="flex flex-col gap-2">
                <span>Someone else changed the settings since you loaded them, so nothing was saved. Reload the latest settings and save again; your edits in this section can be kept.</span>
                <div class="flex gap-2">
                  <BaseButton size="sm" variant="primary" @click="reloadKeeping">Reload, keep my edits</BaseButton>
                  <BaseButton size="sm" @click="discard">Discard my edits</BaseButton>
                </div>
              </div>
            </BaseAlert>
            <BaseAlert v-if="errors.general && !hasOnlyFieldErrors('general')" :error="errors.general" />
            <FormField label="Product name" hint="Shown in the browser title, the app header and email." :error="fe('general', 'product.name')" required>
              <TextInput v-model="general.name" autocomplete="off" />
            </FormField>
            <FormField label="Description" optional hint="What this installation is for (up to 1000 characters)." :error="fe('general', 'product.description')">
              <TextArea v-model="general.description" :rows="3" />
            </FormField>
            <FormField label="Reply signature" optional hint="Markdown, up to 1000 characters. The server adds it after each agent's own signature on their public replies; it is not shown in the reply box. Never added to notes or integration messages." :error="fe('general', 'product.signature')">
              <TextArea v-model="general.signature" :rows="3" />
            </FormField>
            <FormField
              label="Base URL"
              hint="The address staff open, e.g. https://support.example.com. Used for links in staff email and for sign-in origin checks; email cannot be enabled without it."
              :error="fe('general', 'product.baseUrl')"
            >
              <TextInput v-model="general.baseUrl" type="url" mono autocomplete="off" />
            </FormField>
            <BaseAlert v-if="server.product.baseUrl && general.baseUrl.trim() !== server.product.baseUrl" tone="warn">
              Changing the base URL changes every emailed link and which browser origins may refresh sessions.
            </BaseAlert>
            <FormField
              label="Requester ticket link"
              optional
              hint="The system that creates tickets. Requester emails link to <this address>/<ticket number>; leave empty to send no link (the email then asks the requester to check the system they used). Requester emails never include message content."
              :error="fe('general', 'notifications.requesterLinkBaseUrl')"
            >
              <TextInput v-model="general.requesterLink" type="url" mono autocomplete="off" placeholder="https://portal.example.com/requests" />
            </FormField>
            <p v-if="requesterExample" class="m-0 text-xs text-muted">Example for ticket #1042: <span class="lg-mono break-all">{{ requesterExample }}</span></p>
            <div class="flex items-center gap-3">
              <BaseButton type="submit" variant="primary" :loading="saving === 'general'" :disabled="!generalDirty">Save</BaseButton>
              <span v-if="saved === 'general'" role="status" class="text-xs text-muted">Saved.</span>
            </div>
          </form>
        </section>

        <!-- Email -->
        <section v-show="activeSection === 'email'" id="email" tabindex="-1" aria-labelledby="email-h" class="lg-panel flex scroll-mt-4 flex-col gap-4 p-4 outline-none">
          <div>
            <h2 id="email-h" class="lg-h3">Email</h2>
            <p class="m-0 mt-0.5 text-muted">Outgoing email for staff notifications and requester notices.</p>
          </div>
          <form class="flex flex-col gap-4" novalidate @submit.prevent="save('email')">
            <BaseAlert v-if="conflict === 'email'" tone="warn">
              <div class="flex flex-col gap-2">
                <span>Someone else changed the settings since you loaded them, so nothing was saved. Reload the latest settings and save again; your edits in this section can be kept.</span>
                <div class="flex gap-2">
                  <BaseButton size="sm" variant="primary" @click="reloadKeeping">Reload, keep my edits</BaseButton>
                  <BaseButton size="sm" @click="discard">Discard my edits</BaseButton>
                </div>
              </div>
            </BaseAlert>
            <BaseAlert v-if="errors.email && !hasOnlyFieldErrors('email')" :error="errors.email" />
            <div class="flex items-start gap-3">
              <SwitchInput id="smtp-enabled" v-model="smtp.enabled" labelledby="smtp-enabled-l" describedby="smtp-enabled-d" class="mt-0.5" />
              <div class="flex min-w-0 flex-col">
                <span class="flex items-center gap-2"><strong id="smtp-enabled-l">Send email</strong><BaseBadge :tone="smtp.enabled ? 'success' : 'neutral'">{{ smtp.enabled ? 'On' : 'Off' }}</BaseBadge></span>
                <span id="smtp-enabled-d" class="text-xs text-muted">
                  Needs a server, a sender address and the base URL (General).
                  <template v-if="!smtp.enabled">While off, notifications are recorded as “Not configured” in the delivery log instead of being sent.</template>
                </span>
              </div>
            </div>
            <span v-if="fe('email', 'smtp.enabled')" class="lg-field-error">{{ fe('email', 'smtp.enabled') }}</span>

            <div class="flex flex-col gap-3 border-0 border-t border-solid border-line pt-3">
              <h3 class="m-0 text-xs font-semibold uppercase tracking-wider text-muted">Server</h3>
              <div class="grid grid-cols-[minmax(0,2fr)_minmax(0,1fr)] gap-3 max-sm:grid-cols-1">
                <FormField label="Host" :error="fe('email', 'smtp.host')">
                  <TextInput v-model="smtp.host" mono autocomplete="off" placeholder="smtp.example.com" />
                </FormField>
                <FormField label="Port" :error="fe('email', 'smtp.port')">
                  <TextInput v-model="smtp.port" type="number" mono autocomplete="off" />
                </FormField>
              </div>
              <FormField label="Security" hint="Server certificates are verified unless you skip verification below." :error="fe('email', 'smtp.tlsMode')">
                <SelectInput v-model="smtp.tlsMode" :options="tlsOptions" />
              </FormField>
              <FormField label="Custom CA certificate (PEM)" optional hint="For a relay with a self-signed or private CA certificate. Ignored and cleared when TLS is None." :error="fe('email', 'smtp.caCertificate')">
                <TextArea v-model="smtp.caCertificate" :rows="4" :disabled="smtp.tlsMode === 'none'" class="lg-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
              </FormField>
              <div class="flex items-start gap-3">
                <SwitchInput id="smtp-skip-verify" v-model="smtp.tlsSkipVerify" :disabled="smtp.tlsMode === 'none'" labelledby="smtp-skip-verify-l" describedby="smtp-skip-verify-d" class="mt-0.5" />
                <div class="flex min-w-0 flex-col">
                  <strong id="smtp-skip-verify-l">Skip certificate verification</strong>
                  <span id="smtp-skip-verify-d" class="text-xs text-warn">Not recommended: anyone between this server and the relay could read or change mail and see the SMTP password.</span>
                </div>
              </div>
              <div class="grid grid-cols-2 gap-3 max-sm:grid-cols-1">
                <FormField label="Username" optional :error="fe('email', 'smtp.username')">
                  <TextInput v-model="smtp.username" autocomplete="off" />
                </FormField>
                <FormField label="Password" :required="passwordRequired && passwordMode === 'keep'" :error="fe('email', 'smtp.password')" :hint="passwordRequired && passwordMode === 'keep' ? 'Enter the password again: you changed where or how it is sent.' : 'Write-only: stored encrypted and never shown again.'">
                  <template #label-extra>
                    <BaseBadge v-if="server.smtp.passwordConfigured && passwordMode !== 'clear'" tone="success">Saved</BaseBadge>
                    <BaseBadge v-else-if="!server.smtp.passwordConfigured">Not set</BaseBadge>
                  </template>
                  <div v-if="server.smtp.passwordConfigured && passwordMode === 'keep'" class="flex gap-2">
                    <BaseButton size="sm" @click="passwordMode = 'replace'"><template #icon><IconPencil :size="14" stroke="1.75" aria-hidden="true" /></template>Replace password</BaseButton>
                    <BaseButton size="sm" variant="danger" @click="passwordMode = 'clear'"><template #icon><IconTrash :size="14" stroke="1.75" aria-hidden="true" /></template>Clear password</BaseButton>
                  </div>
                  <div v-else-if="passwordMode === 'clear'" class="flex items-center gap-2 text-xs">
                    <span class="text-warn">The stored password will be removed when you save.</span>
                    <BaseButton size="sm" variant="ghost" @click="passwordMode = 'keep'">Undo</BaseButton>
                  </div>
                  <div v-else class="flex items-center gap-2">
                    <div class="flex-1"><PasswordInput v-model="password" autocomplete="new-password" @update:model-value="passwordMode = 'replace'" /></div>
                    <BaseButton v-if="server.smtp.passwordConfigured" size="sm" variant="ghost" @click="passwordMode = 'keep'; password = ''">Keep current</BaseButton>
                  </div>
                </FormField>
              </div>
            </div>

            <div class="flex flex-col gap-3 border-0 border-t border-solid border-line pt-3">
              <h3 class="m-0 text-xs font-semibold uppercase tracking-wider text-muted">Sender</h3>
              <div class="grid grid-cols-[minmax(0,2fr)_minmax(0,1fr)] gap-3 max-sm:grid-cols-1">
                <FormField label="From address" :error="fe('email', 'smtp.fromAddress')">
                  <TextInput v-model="smtp.fromAddress" type="email" autocomplete="off" placeholder="support@example.com" />
                </FormField>
                <FormField label="From name" optional :error="fe('email', 'smtp.fromName')">
                  <TextInput v-model="smtp.fromName" autocomplete="off" />
                </FormField>
              </div>
            </div>

            <div class="flex flex-col gap-3 border-0 border-t border-solid border-line pt-3">
              <h3 class="m-0 text-xs font-semibold uppercase tracking-wider text-muted">Requester notices</h3>
              <BaseAlert v-if="project.error.value" :error="project.error.value" />
              <div class="flex items-start gap-3">
                <SwitchInput id="ack" v-model="ack" :disabled="!project.data.value" labelledby="ack-l" describedby="ack-d" class="mt-0.5" />
                <div class="flex min-w-0 flex-col">
                  <strong id="ack-l">Confirm new tickets</strong>
                  <span id="ack-d" class="text-xs text-muted">Email the requester “we received your request” when a ticket is created by an agent or an integration. The email has no link, and replies to it are not processed.</span>
                </div>
              </div>
            </div>

            <div class="flex flex-wrap items-center gap-3">
              <BaseButton type="submit" variant="primary" :loading="saving === 'email'" :disabled="!emailDirty">Save</BaseButton>
              <BaseButton aria-haspopup="dialog" @click="openTest">
                <template #icon><IconSend :size="16" stroke="1.75" aria-hidden="true" /></template>
                Send test email…
              </BaseButton>
              <span v-if="saved === 'email'" role="status" class="text-xs text-muted">Saved.</span>
            </div>
          </form>
        </section>

        <AutomationSettings v-show="activeSection === 'rules'" @count="rulesOn = $event" />

        <!-- Security -->
        <section v-show="activeSection === 'security'" id="security" tabindex="-1" aria-labelledby="security-h" class="lg-panel flex scroll-mt-4 flex-col gap-4 p-4 outline-none">
          <div>
            <h2 id="security-h" class="lg-h3">Security</h2>
            <p class="m-0 mt-0.5 text-muted">Sign-in rules for staff.</p>
          </div>
          <form class="flex flex-col gap-3" novalidate @submit.prevent="save('auth')">
            <BaseAlert v-if="conflict === 'auth'" tone="warn">
              <div class="flex flex-col gap-2">
                <span>Someone else changed the settings since you loaded them, so nothing was saved. Reload the latest settings and save again; your edits in this section can be kept.</span>
                <div class="flex gap-2">
                  <BaseButton size="sm" variant="primary" @click="reloadKeeping">Reload, keep my edits</BaseButton>
                  <BaseButton size="sm" @click="discard">Discard my edits</BaseButton>
                </div>
              </div>
            </BaseAlert>
            <BaseAlert v-if="errors.auth && !hasOnlyFieldErrors('auth')" :error="errors.auth" />
            <div class="flex items-start gap-3">
              <SwitchInput id="mfa" v-model="requireAdminMfa" labelledby="mfa-l" describedby="mfa-d" class="mt-0.5" />
              <div class="flex min-w-0 flex-col">
                <span class="flex items-center gap-2"><strong id="mfa-l">Require two-step verification for admins</strong><BaseBadge :tone="requireAdminMfa ? 'success' : 'neutral'">{{ requireAdminMfa ? 'On' : 'Off' }}</BaseBadge></span>
                <span id="mfa-d" class="text-xs text-muted">Admins without an authenticator app must set one up before they can use anything else (web app, API and MCP). Agents are not affected.</span>
              </div>
            </div>
            <span v-if="fe('auth', 'auth.requireAdminMfa')" class="lg-field-error">{{ fe('auth', 'auth.requireAdminMfa') }}</span>
            <BaseAlert v-if="session.user && !session.user.mfaEnabled" tone="warn">
              You don’t have two-step verification yet. Turning this on asks you to set it up right away.
            </BaseAlert>
            <BaseAlert v-if="requireAdminMfa && !server.auth.requireAdminMfa" tone="warn">
              <span class="flex flex-col gap-1">
                <span>After saving, every administrator without two-step verification<template v-if="session.user && !session.user.mfaEnabled">, including you,</template> is limited to setting it up; their other pages and API calls are refused until they do.</span>
                <span>Keep recovery codes safe: an administrator who loses both their device and their codes needs another administrator to reset it under Users.</span>
              </span>
            </BaseAlert>
            <BaseAlert v-else-if="!requireAdminMfa && !server.auth.requireAdminMfa" tone="info">
              Off: an administrator account is protected by its password alone. Turn this on before this installation is reachable from the internet.
            </BaseAlert>
            <div class="flex items-center gap-3">
              <BaseButton type="submit" variant="primary" :loading="saving === 'auth'" :disabled="!authDirty">Save</BaseButton>
              <span v-if="saved === 'auth'" role="status" class="text-xs text-muted">Saved.</span>
            </div>
          </form>
        </section>
      </div>
    </div>

    <BaseModal v-model:open="testOpen" title="Send test email" description="Sends one message with the saved settings through the background worker and reports the delivery result." width="md">
      <form id="test-email-form" class="flex flex-col gap-3" novalidate @submit.prevent="sendTest">
        <BaseAlert v-if="smtpDirty" tone="warn">You have unsaved email changes; the test uses the saved settings.</BaseAlert>
        <BaseAlert v-if="testError && !(testError instanceof ApiError && testError.field('to'))" :error="testError" />
        <FormField label="Recipient" :error="testError instanceof ApiError ? testError.field('to') : undefined" required>
          <TextInput v-model="testTo" type="email" autocomplete="off" placeholder="you@example.com" />
        </FormField>
        <DeliveryResult v-if="poll.delivery.value" :delivery="poll.delivery.value" :polling="poll.polling.value" :gave-up="poll.gaveUp.value" />
        <BaseAlert v-if="poll.error.value" :error="poll.error.value" />
      </form>
      <template #footer>
        <BaseButton @click="testOpen = false">Close</BaseButton>
        <BaseButton type="submit" form="test-email-form" variant="primary" :loading="testBusy" :disabled="!testTo.trim()">
          <template #icon><IconSend :size="16" stroke="1.75" aria-hidden="true" /></template>
          Send test email
        </BaseButton>
      </template>
    </BaseModal>
  </AdminPage>
</template>
