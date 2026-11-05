<script setup lang="ts">
// Sign-in (Ledger). Step 1: email or username + password. Accounts with TOTP MFA get a short-lived challenge
// instead of a session; step 2 sends a 6-digit code or a recovery code to POST /auth/mfa.
// An administrator who must enrol (`mfaEnrolmentRequired`) is sent to Account → Security.
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, errorMessage, retryHint } from '@/api/errors'
import { BaseAlert, BaseButton, BaseCard, FormField, LinkButton, PasswordInput, TextInput } from '@/components/base'
import AuthFrame from '@/layouts/AuthFrame.vue'
import { useSessionStore, type MfaChallenge } from '@/stores/session'

const session = useSessionStore()
const router = useRouter()
const route = useRoute()

/** Email address or username; the server decides by the presence of "@". */
const identifier = ref('')
const password = ref('')
const submitting = ref(false)
const error = ref<unknown>(null)
/** Shown above the form after the MFA step had to start over. */
const restartNotice = ref<string | null>(null)

const fieldError = (field: string) => (error.value instanceof ApiError ? error.value.field(field) : undefined)
const formError = computed(() => {
  const err = error.value
  if (!err) return null
  if (err instanceof ApiError) {
    if (err.fieldErrors.length && err.fieldErrors.every((f) => f.field === 'login' || f.field === 'password')) return null
    if (err.status === 401) return 'The email, username or password is incorrect.'
    if (err.status === 429) return `Too many sign-in attempts. ${retryHint(err, 'Wait a moment and try again.')}`
  }
  return errorMessage(err)
})
const notice = computed(() => restartNotice.value ?? (session.expired ? 'Your session ended. Sign in again to continue.' : session.bootstrapError))

async function finish() {
  if (session.mfaEnrolmentRequired) {
    await router.replace({ name: 'account-security' })
    return
  }
  const r = route.query.redirect
  // Only same-app paths; never an absolute or protocol-relative URL.
  const redirect = typeof r === 'string' && r.startsWith('/') && !r.startsWith('//') ? r : '/'
  await router.replace(redirect)
}

async function submit() {
  if (submitting.value || !identifier.value.trim() || !password.value) return
  submitting.value = true
  error.value = null
  restartNotice.value = null
  try {
    const next = await session.login(identifier.value.trim(), password.value)
    password.value = ''
    if (next) {
      startMfa(next)
      return
    }
    await finish()
  } catch (err) {
    error.value = err
  } finally {
    submitting.value = false
  }
}

// ---- Second step ----
const challenge = ref<MfaChallenge | null>(null)
const useRecovery = ref(false)
const code = ref('')
const recoveryCode = ref('')
const mfaError = ref<unknown>(null)
const codeInput = ref<InstanceType<typeof TextInput> | null>(null)
const recoveryInput = ref<InstanceType<typeof TextInput> | null>(null)

const normalizedCode = computed(() => code.value.replace(/\s+/g, ''))
const codeReady = computed(() => /^\d{6}$/.test(normalizedCode.value))
// Recovery codes look like abcd-efgh-ijkl (base32, any case; dashes optional).
const recoveryReady = computed(() => /^[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}$/.test(recoveryCode.value.trim()))

async function focusFactor() {
  await nextTick()
  if (useRecovery.value) recoveryInput.value?.focus()
  else codeInput.value?.focus()
}

function startMfa(c: MfaChallenge) {
  challenge.value = c
  useRecovery.value = false
  code.value = recoveryCode.value = ''
  mfaError.value = null
  void focusFactor()
}

function toggleRecovery() {
  useRecovery.value = !useRecovery.value
  mfaError.value = null
  void focusFactor()
}

function backToPassword(message: string | null = null) {
  challenge.value = null
  code.value = recoveryCode.value = ''
  mfaError.value = null
  restartNotice.value = message
}

const mfaFieldError = computed(() => {
  const err = mfaError.value
  if (!(err instanceof ApiError)) return undefined
  if (err.is('invalidMfaCode')) {
    return useRecovery.value
      ? 'That recovery code is not valid or has already been used.'
      : 'That code is not valid. Use the newest code from your app and check that the device clock is correct.'
  }
  return err.field(useRecovery.value ? 'recoveryCode' : 'code')
})
const mfaFormError = computed(() => {
  const err = mfaError.value
  if (!err || mfaFieldError.value) return null
  if (err instanceof ApiError && err.status === 429) return `Too many wrong codes. ${retryHint(err, 'Wait 15 minutes before trying again.')}`
  return errorMessage(err)
})

async function submitMfa() {
  const c = challenge.value
  if (!c || submitting.value) return
  if (useRecovery.value ? !recoveryReady.value : !codeReady.value) return
  submitting.value = true
  mfaError.value = null
  try {
    await session.completeMfa(c, useRecovery.value ? { recoveryCode: recoveryCode.value.trim() } : { code: normalizedCode.value })
    challenge.value = null
    await finish()
  } catch (err) {
    if (err instanceof ApiError && err.is('mfaChallengeInvalid')) {
      backToPassword('The sign-in attempt expired or had too many wrong codes. Sign in again.')
    } else {
      mfaError.value = err
      if (!useRecovery.value) code.value = ''
      void focusFactor()
    }
  } finally {
    submitting.value = false
  }
}

// Authenticator apps and OS autofill fill all six digits at once: submit without an extra click.
watch(codeReady, (ready) => {
  if (ready && challenge.value && !useRecovery.value && !submitting.value) void submitMfa()
})
</script>

<template>
  <AuthFrame v-if="!challenge" title="Sign in" description="Use the account your workspace administrator created for you.">
    <template #notice><BaseAlert v-if="notice" tone="warn" :error="notice" /></template>

    <BaseCard as="div" class="!gap-4 !p-6">
      <form class="flex flex-col gap-4" novalidate @submit.prevent="submit">
        <BaseAlert v-if="formError" :error="formError" />
        <FormField label="Email or username" :error="fieldError('login')" required>
          <TextInput v-model="identifier" name="username" autocomplete="username" autocapitalize="off" spellcheck="false" size="lg" />
        </FormField>
        <FormField label="Password" :error="fieldError('password')" required>
          <template #label-extra>
            <RouterLink :to="{ name: 'forgot-password' }" class="text-[13px] text-muted hover:text-text hover:underline">Forgot password?</RouterLink>
          </template>
          <PasswordInput v-model="password" name="password" autocomplete="current-password" size="lg" />
        </FormField>
        <BaseButton type="submit" variant="primary" size="lg" block :loading="submitting" :disabled="!identifier.trim() || !password">
          {{ submitting ? 'Signing in…' : 'Sign in' }}
        </BaseButton>
      </form>
    </BaseCard>
  </AuthFrame>

  <AuthFrame
    v-else
    title="Two-step verification"
    :description="useRecovery ? 'Enter one of the recovery codes you saved when you set up two-step verification. Each code works once.' : 'Enter the 6-digit code from your authenticator app.'"
  >
    <BaseCard as="div" class="!gap-4 !p-6">
      <form class="flex flex-col gap-4" novalidate @submit.prevent="submitMfa">
        <BaseAlert v-if="mfaFormError" :error="mfaFormError" />
        <FormField v-if="!useRecovery" label="Authentication code" :error="mfaFieldError" required>
          <TextInput
            ref="codeInput"
            v-model="code"
            name="one-time-code"
            autocomplete="one-time-code"
            inputmode="numeric"
            pattern="[0-9]*"
            maxlength="7"
            size="lg"
            mono
            class="tracking-[0.3em]"
            placeholder="123456"
          />
        </FormField>
        <FormField v-else label="Recovery code" :error="mfaFieldError" required>
          <TextInput
            ref="recoveryInput"
            v-model="recoveryCode"
            name="recovery-code"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
            maxlength="20"
            size="lg"
            mono
            placeholder="abcd-efgh-ijkl"
          />
        </FormField>
        <BaseButton type="submit" variant="primary" size="lg" block :loading="submitting" :disabled="useRecovery ? !recoveryReady : !codeReady">
          {{ submitting ? 'Verifying…' : 'Verify' }}
        </BaseButton>
        <div class="flex items-center justify-between gap-3 text-[13px]">
          <LinkButton tone="muted" @click="toggleRecovery">
            {{ useRecovery ? 'Use an authentication code' : 'Use a recovery code' }}
          </LinkButton>
          <LinkButton tone="muted" @click="backToPassword()">Back to sign in</LinkButton>
        </div>
      </form>
    </BaseCard>
    <p class="m-0 text-xs text-muted">Lost your device and your recovery codes? An administrator can reset two-step verification for your account.</p>
  </AuthFrame>
</template>
