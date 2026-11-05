<script setup lang="ts">
// Account → Security: change password and manage TOTP two-step verification (enrol with a
// locally rendered QR code, one-time recovery codes, regenerate, turn off).
import { IconCopy, IconPencil, IconRefresh, IconShieldCheck } from '@tabler/icons-vue'
import { computed, nextTick, onMounted, ref, shallowRef } from 'vue'
import { meApi } from '@/api/endpoints'
import { ApiError, retryHint } from '@/api/errors'
import type { MfaStatus, MfaStepUp, NameParts, TotpEnrolment } from '@/api/types'
import {
  BaseAlert,
  BaseBadge,
  BaseButton,
  BaseCard,
  CodeBlock,
  FormField,
  LoadingState,
  PasswordInput,
  RelativeTime,
  TextInput,
} from '@/components/base'
import NameFields from '@/components/account/NameFields.vue'
import NewPasswordFields from '@/components/account/NewPasswordFields.vue'
import PasswordConfirmDialog from '@/components/account/PasswordConfirmDialog.vue'
import RecoveryCodesPanel from '@/components/account/RecoveryCodesPanel.vue'
import TotpQrCode from '@/components/account/TotpQrCode.vue'
import { newPasswordProblem } from '@/password'
import { useSessionStore } from '@/stores/session'
import AdminPage from '../admin/AdminPage.vue'

const session = useSessionStore()

// ---- Name (self-service; the derived display name is shown on tickets, notes and emails) ----
const nameEditing = ref(false)
const nameDraft = ref<NameParts>({ givenName: '', middleName: '', familyName: '' })
const nameBusy = ref(false)
const nameError = ref<unknown>(null)
const nameFe = (f: string): string | undefined => (nameError.value instanceof ApiError ? nameError.value.field(f) : undefined)
function editName() {
  const u = session.user
  if (!u) return
  nameDraft.value = { givenName: u.givenName, middleName: u.middleName, familyName: u.familyName }
  nameError.value = null
  nameEditing.value = true
}
async function saveName() {
  const u = session.user
  if (!u) return
  nameBusy.value = true
  nameError.value = null
  const d = nameDraft.value
  try {
    session.setUser(
      await meApi.updateProfile(u, { givenName: d.givenName.trim(), middleName: d.middleName.trim(), familyName: d.familyName.trim() }),
    )
    nameEditing.value = false
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      await session.refreshUser()
      nameError.value = 'Your account was changed elsewhere. The latest version is loaded; save again to apply your text.'
    } else nameError.value = err
  } finally {
    nameBusy.value = false
  }
}

// ---- MFA status ----
const status = shallowRef<MfaStatus | null>(null)
const statusError = ref<unknown>(null)
const loading = ref(false)

async function loadStatus() {
  loading.value = true
  statusError.value = null
  try {
    status.value = await meApi.mfa()
  } catch (err) {
    statusError.value = err
  } finally {
    loading.value = false
  }
}
onMounted(loadStatus)

// ---- Enrolment: password → scan + confirm code → recovery codes ----
type Step = 'idle' | 'password' | 'scan' | 'codes'
const step = ref<Step>('idle')
const enrolPassword = ref('')
const enrolBusy = ref(false)
const enrolError = ref<unknown>(null)
const enrolment = shallowRef<TotpEnrolment | null>(null)
const confirmCode = ref('')
const recoveryCodes = ref<string[] | null>(null)
const passwordInputWrap = ref<HTMLElement | null>(null)
const codeInput = ref<InstanceType<typeof TextInput> | null>(null)

const enrolFieldError = (f: string) => {
  const e = enrolError.value
  if (!(e instanceof ApiError)) return undefined
  const msg = e.field(f)
  if (!msg) return undefined
  if (f === 'password' && e.fieldErrors.some((x) => x.field === 'password' && x.code === 'incorrect')) return 'That password is not correct.'
  if (f === 'code' && e.fieldErrors.some((x) => x.field === 'code' && x.code === 'incorrect')) {
    return 'That code did not match. Enter the newest code, and check that the device clock is set automatically.'
  }
  return msg
}
const enrolFormError = computed(() => {
  const e = enrolError.value
  if (!e) return null
  if (e instanceof ApiError) {
    if (e.fieldErrors.length) return null
    if (e.status === 429) return `Too many attempts. ${retryHint(e, 'Wait a few minutes and try again.')}`
  }
  return e
})

async function beginEnrol() {
  step.value = 'password'
  enrolPassword.value = ''
  enrolError.value = null
  await nextTick()
  passwordInputWrap.value?.querySelector('input')?.focus()
}

function cancelEnrol() {
  step.value = 'idle'
  enrolPassword.value = confirmCode.value = ''
  enrolment.value = null
  enrolError.value = null
  void loadStatus()
}

async function submitEnrolPassword() {
  if (!enrolPassword.value || enrolBusy.value) return
  enrolBusy.value = true
  enrolError.value = null
  try {
    enrolment.value = await meApi.startTotp(enrolPassword.value)
    enrolPassword.value = ''
    confirmCode.value = ''
    step.value = 'scan'
    await nextTick()
    codeInput.value?.focus()
  } catch (err) {
    enrolError.value = err
  } finally {
    enrolBusy.value = false
  }
}

const normalizedCode = computed(() => confirmCode.value.replace(/\s+/g, ''))
const codeReady = computed(() => /^\d{6}$/.test(normalizedCode.value))

async function submitConfirm() {
  if (!codeReady.value || enrolBusy.value) return
  enrolBusy.value = true
  enrolError.value = null
  try {
    const res = await meApi.confirmTotp(normalizedCode.value)
    enrolment.value = null
    confirmCode.value = ''
    recoveryCodes.value = res.recoveryCodes
    step.value = 'codes'
  } catch (err) {
    enrolError.value = err
    // 409: no pending enrolment any more (expired or replaced) → start again.
    if (err instanceof ApiError && err.status === 409) {
      enrolment.value = null
      step.value = 'idle'
      void loadStatus()
    }
  } finally {
    enrolBusy.value = false
  }
}

const secretGroups = computed(() => enrolment.value?.secret.match(/.{1,4}/g)?.join(' ') ?? '')
const issuer = computed(() => {
  const uri = enrolment.value?.otpauthUri
  if (!uri) return ''
  try {
    return new URL(uri).searchParams.get('issuer') ?? ''
  } catch {
    return ''
  }
})
const secretCopied = ref(false)
async function copySecret() {
  if (!enrolment.value) return
  try {
    await navigator.clipboard.writeText(enrolment.value.secret)
    secretCopied.value = true
    setTimeout(() => (secretCopied.value = false), 2000)
  } catch {
    // Clipboard blocked (insecure origin); the key is selectable.
  }
}

/** Recovery codes acknowledged: forget them, refresh status and lift the enrolment gate. */
async function codesSaved() {
  recoveryCodes.value = null
  step.value = 'idle'
  await loadStatus()
  try {
    await session.refreshUser()
  } catch (err) {
    statusError.value = err
  }
}

// ---- Regenerate / turn off ----
const regenOpen = ref(false)
const disableOpen = ref(false)
const actionNotice = ref<string | null>(null)

async function regenerate(password: string, code: MfaStepUp) {
  const res = await meApi.regenerateRecoveryCodes(password, code)
  recoveryCodes.value = res.recoveryCodes
  step.value = 'codes'
  actionNotice.value = null
}

async function disable(password: string, code: MfaStepUp) {
  try {
    await meApi.disableMfa(password, code)
  } catch (err) {
    if (err instanceof ApiError && err.is('mfaRequiredByPolicy')) {
      throw new Error('Administrators must keep two-step verification on while the installation requires it.')
    }
    throw err
  }
  actionNotice.value = 'Two-step verification is off. You will sign in with your password only.'
  await loadStatus()
  try {
    await session.refreshUser()
  } catch {
    // Status above is authoritative for this page.
  }
}

// ---- Change password ----
const current = ref('')
const next = ref('')
const confirmNext = ref('')
const pwBusy = ref(false)
const pwError = ref<unknown>(null)
const pwDone = ref(false)
const pwProblem = computed(() => (current.value ? newPasswordProblem(next.value, confirmNext.value) : 'Enter your current password.'))
const pwField = (f: string) => {
  const e = pwError.value
  if (!(e instanceof ApiError)) return undefined
  if (f === 'currentPassword' && e.fieldErrors.some((x) => x.field === f && x.code === 'incorrect')) return 'That password is not correct.'
  return e.field(f)
}
const pwFormError = computed(() => {
  const e = pwError.value
  if (!e) return null
  if (e instanceof ApiError) {
    if (e.fieldErrors.length) return null
    if (e.status === 429) return `Too many attempts. ${retryHint(e, 'Wait a few minutes and try again.')}`
  }
  return e
})

async function changePassword() {
  if (pwProblem.value || pwBusy.value) return
  pwBusy.value = true
  pwError.value = null
  pwDone.value = false
  try {
    await meApi.changePassword(current.value, next.value)
    current.value = next.value = confirmNext.value = ''
    pwDone.value = true
  } catch (err) {
    pwError.value = err
  } finally {
    pwBusy.value = false
  }
}
</script>

<template>
  <AdminPage section="Account" title="Security" description="Your password and two-step verification. These settings apply to your account only.">
    <BaseAlert v-if="session.mfaEnrolmentRequired" tone="warn" class="max-w-2xl">
      This installation requires administrators to use two-step verification. Set it up below to continue; other pages
      are unavailable until you do.
    </BaseAlert>

    <BaseCard title="Profile and sign-in" class="max-w-2xl">
      <dl class="m-0 grid grid-cols-[180px_1fr] items-start gap-x-4 gap-y-1.5">
        <dt class="text-muted">Name</dt>
        <dd class="m-0 flex min-w-0 items-baseline gap-2">
          <span class="lg-ell">{{ session.user?.name }}</span>
          <BaseButton v-if="!nameEditing" size="sm" variant="ghost" @click="editName"><template #icon><IconPencil :size="14" stroke="1.75" aria-hidden="true" /></template>Edit name</BaseButton>
        </dd>
        <dt class="text-muted">Email</dt>
        <dd class="lg-ell m-0">{{ session.user?.email }}</dd>
        <dt class="text-muted">Username</dt>
        <dd class="lg-ell lg-mono m-0">{{ session.user?.username || '—' }}</dd>
      </dl>
      <form v-if="nameEditing" class="flex flex-col gap-3" novalidate @submit.prevent="saveName">
        <BaseAlert v-if="nameError && !(nameError instanceof ApiError && nameError.fieldErrors.length)" :error="nameError" />
        <NameFields v-model="nameDraft" :errors="nameFe" size="sm" />
        <div class="flex gap-2">
          <BaseButton type="submit" variant="primary" size="sm" :loading="nameBusy" :disabled="!nameDraft.givenName.trim() || !nameDraft.familyName.trim()">Save name</BaseButton>
          <BaseButton size="sm" @click="nameEditing = false">Cancel</BaseButton>
        </div>
      </form>
      <p v-else-if="session.user && !session.user.familyName" class="m-0 text-xs text-muted">Your account has no last name yet. Use “Edit name” to add it.</p>
      <p class="m-0 text-xs text-muted">Sign in with your email or username. Your name is shown to other agents; an administrator can change your username.</p>
    </BaseCard>

    <BaseCard class="max-w-2xl">
      <div class="flex items-center gap-2">
        <h2 class="lg-h3 flex-1">Two-step verification</h2>
        <template v-if="status">
          <BaseBadge v-if="status.enabled" tone="success">
            <template #icon><IconShieldCheck :size="12" stroke="2" aria-hidden="true" /></template>
            On
          </BaseBadge>
          <BaseBadge v-else :tone="status.required ? 'warn' : 'neutral'">Off</BaseBadge>
        </template>
      </div>

      <BaseAlert v-if="statusError" :error="statusError" />
      <LoadingState v-if="loading && !status" />
      <BaseAlert v-if="actionNotice" tone="info">{{ actionNotice }}</BaseAlert>

      <template v-if="status">
        <!-- Recovery codes (after enrolment or regeneration): shown once -->
        <RecoveryCodesPanel v-if="step === 'codes' && recoveryCodes" :codes="recoveryCodes" @done="codesSaved" />

        <!-- Enabled -->
        <template v-else-if="status.enabled && step === 'idle'">
          <p class="m-0 text-muted">
            Signing in asks for a code from your authenticator app after your password.
          </p>
          <dl class="m-0 grid grid-cols-[180px_1fr] gap-x-4 gap-y-1.5">
            <dt class="text-muted">Turned on</dt>
            <dd class="m-0"><RelativeTime v-if="status.enabledAt" :iso="status.enabledAt" /><span v-else>—</span></dd>
            <dt class="text-muted">Recovery codes left</dt>
            <dd class="m-0" :class="status.recoveryCodesRemaining <= 2 ? 'font-medium text-warn' : ''">
              {{ status.recoveryCodesRemaining }} of 10
              <span v-if="status.recoveryCodesRemaining <= 2" class="font-normal"> — generate new codes soon</span>
            </dd>
          </dl>
          <div class="flex flex-wrap gap-2">
            <BaseButton @click="regenOpen = true"><template #icon><IconRefresh :size="16" stroke="1.75" aria-hidden="true" /></template>Generate new recovery codes</BaseButton>
            <BaseButton variant="danger" :disabled="status.required" @click="disableOpen = true">Turn off</BaseButton>
          </div>
          <p v-if="status.required" class="m-0 text-xs text-muted">
            Administrators cannot turn it off while the installation requires two-step verification.
          </p>
        </template>

        <!-- Not enabled: start -->
        <template v-else-if="step === 'idle'">
          <p class="m-0 text-muted">
            Protect your account with a 6-digit code from an authenticator app (for example 1Password, Bitwarden,
            Google Authenticator or Microsoft Authenticator) in addition to your password.
          </p>
          <BaseAlert v-if="enrolFormError" :error="enrolFormError" />
          <p v-if="status.pendingEnrolment && status.pendingEnrolmentExpiresAt" class="m-0 text-xs text-muted">
            An unfinished setup <RelativeTime :iso="status.pendingEnrolmentExpiresAt" future-prefix="expires" past-prefix="expired" />.
            Start again to get a new key.
          </p>
          <div><BaseButton variant="primary" @click="beginEnrol">Set up authenticator app</BaseButton></div>
        </template>

        <!-- Step 1: confirm password -->
        <form v-else-if="step === 'password'" class="flex flex-col gap-3" novalidate @submit.prevent="submitEnrolPassword">
          <p class="m-0 text-muted">Step 1 of 3 · Confirm it is you.</p>
          <BaseAlert v-if="enrolFormError" :error="enrolFormError" />
          <div ref="passwordInputWrap" class="max-w-sm">
            <FormField label="Current password" :error="enrolFieldError('password')" required>
              <PasswordInput v-model="enrolPassword" autocomplete="current-password" />
            </FormField>
          </div>
          <div class="flex gap-2">
            <BaseButton type="submit" variant="primary" :loading="enrolBusy" :disabled="!enrolPassword">Continue</BaseButton>
            <BaseButton @click="cancelEnrol">Cancel</BaseButton>
          </div>
        </form>

        <!-- Step 2: scan and confirm -->
        <form v-else-if="step === 'scan' && enrolment" class="flex flex-col gap-4" novalidate @submit.prevent="submitConfirm">
          <p class="m-0 text-muted">
            Step 2 of 3 · Scan the QR code with your authenticator app, then enter the 6-digit code it shows.
            This setup <RelativeTime :iso="enrolment.expiresAt" future-prefix="expires" past-prefix="expired" />.
          </p>
          <div class="flex flex-wrap items-start gap-5">
            <TotpQrCode :uri="enrolment.otpauthUri" />
            <div class="flex min-w-[220px] flex-1 flex-col gap-2">
              <span class="lg-label">Can't scan it? Enter this setup key</span>
              <div class="flex items-center gap-2">
                <CodeBlock inline select-all class="!break-normal">{{ secretGroups }}</CodeBlock>
                <BaseButton size="sm" @click="copySecret">
                  <template #icon><IconCopy :size="14" stroke="1.75" aria-hidden="true" /></template>
                  {{ secretCopied ? 'Copied' : 'Copy' }}
                </BaseButton>
              </div>
              <span class="text-xs text-muted">
                Time-based (TOTP), 6 digits, every 30 seconds<template v-if="issuer">; it appears as “{{ issuer }}”</template>.
                Spaces are only for readability.
              </span>
            </div>
          </div>
          <BaseAlert v-if="enrolFormError" :error="enrolFormError" />
          <div class="max-w-[220px]">
            <FormField label="Code from the app" :error="enrolFieldError('code')" required>
              <TextInput
                ref="codeInput"
                v-model="confirmCode"
                autocomplete="one-time-code"
                inputmode="numeric"
                pattern="[0-9]*"
                maxlength="7"
                mono
                class="tracking-[0.3em]"
                placeholder="123456"
              />
            </FormField>
          </div>
          <div class="flex gap-2">
            <BaseButton type="submit" variant="primary" :loading="enrolBusy" :disabled="!codeReady">Turn on</BaseButton>
            <BaseButton @click="cancelEnrol">Cancel</BaseButton>
          </div>
          <p class="m-0 text-xs text-muted">Step 3 shows your recovery codes. Turning this on signs out your other devices.</p>
        </form>
      </template>
    </BaseCard>

    <BaseCard title="Password" class="max-w-2xl">
      <form class="flex flex-col gap-3" novalidate @submit.prevent="changePassword">
        <BaseAlert v-if="pwFormError" :error="pwFormError" />
        <BaseAlert v-if="pwDone" tone="info">Password changed. Your other devices were signed out; this one stays signed in.</BaseAlert>
        <input type="text" name="username" autocomplete="username" :value="session.user?.email ?? ''" class="hidden" aria-hidden="true" tabindex="-1" readonly />
        <div class="flex max-w-sm flex-col gap-3">
          <FormField label="Current password" :error="pwField('currentPassword')" required>
            <PasswordInput v-model="current" autocomplete="current-password" />
          </FormField>
          <NewPasswordFields v-model="next" v-model:confirm="confirmNext" :error="pwField('newPassword')" />
        </div>
        <div><BaseButton type="submit" variant="primary" :loading="pwBusy" :disabled="!!pwProblem">Change password</BaseButton></div>
      </form>
    </BaseCard>

    <PasswordConfirmDialog
      v-model:open="regenOpen"
      title="Generate new recovery codes"
      description="Your current recovery codes stop working immediately and 10 new ones are shown once."
      confirm-label="Generate codes"
      with-code
      :action="regenerate"
    />
    <PasswordConfirmDialog
      v-model:open="disableOpen"
      title="Turn off two-step verification"
      description="Signing in will need only your password. Your recovery codes stop working."
      confirm-label="Turn off"
      with-code
      danger
      :action="disable"
    />
  </AdminPage>
</template>
