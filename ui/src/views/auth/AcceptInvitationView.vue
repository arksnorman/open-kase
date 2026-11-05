<script setup lang="ts">
// /accept-invitation?token= — an invited user sets a password (and may correct the name the
// administrator entered). The account becomes active; signing in is a separate step.
import { computed, ref } from 'vue'
import { authApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { NameParts } from '@/api/types'
import { BaseAlert, BaseButton, BaseCard } from '@/components/base'
import NameFields from '@/components/account/NameFields.vue'
import NewPasswordFields from '@/components/account/NewPasswordFields.vue'
import { useLinkToken } from '@/composables/useLinkToken'
import AuthFrame from '@/layouts/AuthFrame.vue'
import { newPasswordProblem } from '@/password'
import { useBrandStore } from '@/stores/brand'

const brand = useBrandStore()
const token = useLinkToken()
const name = ref<NameParts>({ givenName: '', middleName: '', familyName: '' })
const password = ref('')
const confirm = ref('')
const submitting = ref(false)
const done = ref(false)
const error = ref<unknown>(null)
const tokenInvalid = ref(token === null)

const problem = computed(() => newPasswordProblem(password.value, confirm.value))
const fe = (f: string): string | undefined => (error.value instanceof ApiError ? error.value.field(f) : undefined)
const onlyFieldErrors = computed(() => error.value instanceof ApiError && error.value.fieldErrors.length > 0)

async function submit() {
  if (!token || submitting.value || problem.value) return
  submitting.value = true
  error.value = null
  try {
    // Any name part sent replaces the whole stored name (first and last name then required).
    const n = name.value
    const given = n.givenName.trim(), middle = n.middleName.trim(), family = n.familyName.trim()
    const corrected = given || middle || family
    await authApi.acceptInvitation({
      token,
      password: password.value,
      ...(corrected ? { givenName: given, familyName: family, ...(middle ? { middleName: middle } : {}) } : {}),
    })
    password.value = confirm.value = ''
    done.value = true
  } catch (err) {
    if (err instanceof ApiError && err.field('token')) tokenInvalid.value = true
    else error.value = err
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AuthFrame
    :title="`Join ${brand.name}`"
    :description="done || tokenInvalid ? undefined : 'You were invited to the support workspace. Choose a password to activate your account.'"
  >
    <BaseCard as="div" class="!gap-4 !p-6">
      <div v-if="done" role="status" class="flex flex-col gap-3">
        <p class="m-0">Your account is active. Sign in with your email address and the password you just chose.</p>
        <BaseButton variant="primary" size="lg" block :to="{ name: 'sign-in' }">Sign in</BaseButton>
      </div>
      <div v-else-if="tokenInvalid" class="flex flex-col gap-3">
        <BaseAlert :error="token ? 'This invitation link is invalid, has already been used, or has expired.' : 'This invitation link is incomplete.'" />
        <p class="m-0 text-muted">Invitations work once and expire after 72 hours. Ask your workspace administrator to send a new one.</p>
        <RouterLink :to="{ name: 'sign-in' }" class="self-center text-[13px] text-muted hover:text-text hover:underline">Go to sign in</RouterLink>
      </div>
      <form v-else class="flex flex-col gap-4" novalidate @submit.prevent="submit">
        <BaseAlert v-if="error && !onlyFieldErrors" :error="error" />
        <fieldset class="m-0 flex flex-col gap-2 border-0 p-0">
          <legend class="lg-label mb-1.5 p-0">Your name <span class="font-normal text-muted">(optional)</span></legend>
          <NameFields v-model="name" :errors="fe" all-optional stacked size="lg" />
          <p class="m-0 text-xs text-muted">Leave empty to keep the name your administrator entered. To correct it, enter at least your first and last name.</p>
        </fieldset>
        <NewPasswordFields v-model="password" v-model:confirm="confirm" label="Password" :error="fe('password')" size="lg" />
        <BaseButton type="submit" variant="primary" size="lg" block :loading="submitting" :disabled="!!problem">
          {{ submitting ? 'Activating…' : 'Activate account' }}
        </BaseButton>
      </form>
    </BaseCard>
  </AuthFrame>
</template>
