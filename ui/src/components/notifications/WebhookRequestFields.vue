<script setup lang="ts">
// Security settings of a webhook endpoint: authentication (None / Basic / Token), extra headers and
// TLS (custom certificate authority, client certificate). Secrets are write-only: stored values are
// shown as "stored" and kept when their field is left empty. The parent calls `toInput()` on submit.
import { IconChevronDown, IconPlus, IconTrash } from '@tabler/icons-vue'
import { computed, ref, watch } from 'vue'
import { ApiError } from '@/api/errors'
import type { Webhook, WebhookAuthType, WebhookInput } from '@/api/types'
import { BaseButton, BaseCard, BaseBadge, FormField, IconButton, PasswordInput, SegmentedControl, TextArea, TextInput } from '@/components/base'
import type { Segment } from '@/components/base/types'

/** `url` is the endpoint URL being edited (the parent's draft), to tell when the destination moved. */
const props = defineProps<{ initial?: Webhook | null; url?: string; error?: unknown; cards?: boolean }>()

interface Row {
  name: string
  value: string
  stored: boolean
}
const rows = ref<Row[]>([])
const authType = ref<WebhookAuthType>('none')
const username = ref('')
const password = ref('')
const token = ref('')
const ca = ref('')
const clientCert = ref('')
const clientKey = ref('')
const tlsOpen = ref(false)

function reset() {
  const w = props.initial
  rows.value = (w?.headers ?? []).map((h) => ({ name: h.name, value: '', stored: h.valueConfigured }))
  authType.value = (w?.auth.type as WebhookAuthType | undefined) ?? 'none'
  username.value = w?.auth.username ?? ''
  password.value = token.value = clientKey.value = ''
  ca.value = w?.tls.caCertificate ?? ''
  clientCert.value = w?.tls.clientCertificate ?? ''
  tlsOpen.value = !!(ca.value || clientCert.value)
}
watch(() => props.initial, reset, { immediate: true })

const authOptions: Segment<WebhookAuthType>[] = [
  { value: 'none', label: 'None' },
  { value: 'basic', label: 'Basic' },
  { value: 'token', label: 'Token' },
]
const storedPassword = computed(() => props.initial?.auth.type === 'basic' && props.initial.auth.passwordConfigured)
const storedToken = computed(() => props.initial?.auth.type === 'token' && props.initial.auth.tokenConfigured)
const storedKey = computed(() => !!props.initial?.tls.clientKeyConfigured)

// Mirrors the server rule: stored secrets are only reused for the destination they were entered for, so
// a new scheme, host or port (or a different custom CA) needs them typed again.
const reenter = computed(() => {
  const w = props.initial
  if (!w) return false
  const newCa = ca.value.trim()
  if (newCa && newCa !== (w.tls.caCertificate ?? '').trim()) return true
  const draft = (props.url ?? w.url).trim()
  if (draft === w.url) return false
  try {
    const a = new URL(w.url)
    const b = new URL(draft)
    return a.protocol !== b.protocol || a.hostname.toLowerCase() !== b.hostname.toLowerCase() || a.port !== b.port
  } catch {
    return true
  }
})
const REENTER_HINT = 'Enter it again: the destination changed.'

const fe = (f: string) => (props.error instanceof ApiError ? props.error.field(f) : undefined)

function addRow() {
  rows.value.push({ name: '', value: '', stored: false })
}
function removeRow(i: number) {
  rows.value.splice(i, 1)
}

/** The headers/auth/tls members to send: everything set on create, only changes on update. */
function toInput(): Pick<WebhookInput, 'headers' | 'auth' | 'tls'> {
  const w = props.initial
  const out: Pick<WebhookInput, 'headers' | 'auth' | 'tls'> = {}
  const headers = rows.value
    .filter((r) => r.name.trim() || r.value)
    .map((r) => (r.value ? { name: r.name.trim(), value: r.value } : { name: r.name.trim() }))
  const before = (w?.headers ?? []).map((h) => h.name).join('\n')
  if (headers.some((h) => 'value' in h) || headers.map((h) => h.name).join('\n') !== before) out.headers = headers

  const a = authType.value
  const authChanged =
    a !== (w?.auth.type ?? 'none') || (a === 'basic' && (username.value !== (w?.auth.username ?? '') || !!password.value)) || (a === 'token' && !!token.value)
  if (authChanged) {
    out.auth = { type: a }
    if (a === 'basic') {
      out.auth.username = username.value
      if (password.value) out.auth.password = password.value
    }
    if (a === 'token' && token.value) out.auth.token = token.value
  }

  const tls: NonNullable<WebhookInput['tls']> = {}
  if (ca.value.trim() !== (w?.tls.caCertificate ?? '')) tls.caCertificate = ca.value.trim()
  if (clientCert.value.trim() !== (w?.tls.clientCertificate ?? '')) tls.clientCertificate = clientCert.value.trim()
  if (clientKey.value.trim()) tls.clientKey = clientKey.value.trim()
  if (Object.keys(tls).length) out.tls = tls
  return out
}
defineExpose({ toInput, reset })
</script>

<template>
  <div class="flex flex-col" :class="cards ? 'gap-4' : 'gap-5'">
    <component :is="cards ? BaseCard : 'section'" v-bind="cards ? { title: 'Authentication' } : { 'aria-labelledby': 'wh-auth-title' }" class="flex flex-col gap-3">
      <h3 v-if="!cards" id="wh-auth-title" class="lg-label m-0">Authentication</h3>
      <SegmentedControl v-model="authType" :options="authOptions" label="Authentication" size="sm" class="self-start" />
      <p v-if="fe('auth.type')" class="lg-field-error m-0">{{ fe('auth.type') }}</p>
      <div v-if="authType === 'basic'" class="grid gap-3 sm:grid-cols-2">
        <FormField label="Username" :error="fe('auth.username')" required>
          <TextInput v-model="username" autocomplete="off" />
        </FormField>
        <FormField label="Password" :error="fe('auth.password') ?? (storedPassword && reenter && !password ? REENTER_HINT : undefined)" :required="!storedPassword || reenter">
          <PasswordInput v-model="password" autocomplete="new-password" :placeholder="storedPassword && !reenter ? 'Saved — leave as is to keep' : ''" />
        </FormField>
      </div>
      <FormField v-else-if="authType === 'token'" label="Token" :error="fe('auth.token') ?? (storedToken && reenter && !token ? REENTER_HINT : undefined)" :required="!storedToken || reenter">
        <PasswordInput v-model="token" autocomplete="new-password" :placeholder="storedToken && !reenter ? 'Saved — leave as is to keep' : ''" />
      </FormField>
      <p v-if="fe('auth')" class="lg-field-error m-0">{{ fe('auth') }}</p>
      <p v-if="reenter && (authType === 'basic' ? storedPassword : authType === 'token' ? storedToken : false)" class="m-0 text-xs text-muted">
        The endpoint moved to a different destination, so the saved credentials are not reused. Enter them again, or choose None.
      </p>
      <p v-if="authType === 'basic' || authType === 'token'" class="m-0 flex items-center gap-2 text-xs text-muted">
        <BaseBadge v-if="authType === 'basic' ? storedPassword : storedToken">Saved</BaseBadge>
        <span>{{ authType === 'basic' ? 'Sent as Authorization: Basic …' : 'Sent as Authorization: Bearer …' }}<template v-if="!reenter && (authType === 'basic' ? storedPassword : storedToken)"> Leave as is to keep it.</template></span>
      </p>
    </component>

    <component :is="cards ? BaseCard : 'fieldset'" v-bind="cards ? { title: 'Extra headers' } : {}" class="flex flex-col gap-2">
      <legend v-if="!cards" class="lg-label mb-1 p-0">Extra headers</legend>
      <p class="m-0 text-xs text-muted" :class="cards ? '-mt-2' : ''">Sent with every request. Values are write-only.</p>
      <p v-if="!rows.length" class="m-0 text-xs text-muted">No extra headers.</p>
      <div v-for="(row, i) in rows" :key="i" class="grid grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)_auto] items-start gap-2">
        <FormField :label="`Header ${i + 1} name`" sr-only-label :error="fe(`headers[${i}].name`)">
          <TextInput v-model="row.name" mono size="sm" autocomplete="off" placeholder="X-Api-Key" />
        </FormField>
        <FormField :label="`Header ${i + 1} value`" sr-only-label :error="fe(`headers[${i}].value`) ?? (row.stored && reenter && !row.value ? REENTER_HINT : undefined)">
          <PasswordInput v-model="row.value" autocomplete="new-password" :placeholder="row.stored && !reenter ? 'Saved — leave as is to keep' : 'Value'" />
        </FormField>
        <IconButton :label="`Remove header ${row.name || i + 1}`" @click="removeRow(i)"><IconTrash :size="16" stroke="1.75" aria-hidden="true" /></IconButton>
      </div>
      <span v-if="fe('headers')" class="lg-field-error">{{ fe('headers') }}</span>
      <div>
        <BaseButton size="sm" variant="ghost" :disabled="rows.length >= 20" @click="addRow">
          <template #icon><IconPlus :size="14" stroke="1.75" aria-hidden="true" /></template>
          Add header
        </BaseButton>
      </div>
    </component>

    <details class="group lg-panel" :open="tlsOpen" @toggle="tlsOpen = ($event.target as HTMLDetailsElement).open">
      <summary class="flex cursor-pointer select-none list-none items-center justify-between gap-2 px-4 py-3 text-[13px] [&::-webkit-details-marker]:hidden">
        <span>Advanced TLS<span class="text-muted">: custom CA, client certificate</span></span>
        <IconChevronDown :size="16" stroke="1.75" aria-hidden="true" class="flex-none text-muted transition-transform group-open:rotate-180" />
      </summary>
      <div class="flex flex-col gap-3 border-t border-border p-4">
        <FormField label="Custom certificate authority" hint="PEM, trusted in addition to system roots." :error="fe('tls.caCertificate')" optional>
          <TextArea v-model="ca" :rows="4" class="lg-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
        </FormField>
        <FormField label="Client certificate" hint="PEM, for mutual TLS. Clear to remove certificate and key." :error="fe('tls.clientCertificate')" optional>
          <TextArea v-model="clientCert" :rows="4" class="lg-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
        </FormField>
        <FormField label="Client private key" :hint="storedKey ? 'Saved. Leave as is to keep it.' : 'PEM, write-only.'" :error="fe('tls.clientKey')" optional>
          <TextArea v-model="clientKey" :rows="4" class="lg-mono text-xs" :placeholder="storedKey ? 'Saved — leave as is to keep' : '-----BEGIN PRIVATE KEY-----'" />
        </FormField>
      </div>
    </details>
  </div>
</template>
