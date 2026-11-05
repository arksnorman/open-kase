<script setup lang="ts">
// /reset-password?token= — choose a new password from an emailed link (single use, 30 minutes).
// Every session of the account is signed out afterwards; MFA stays on.
import { computed, ref } from 'vue'
import { authApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import { BaseAlert, BaseButton, BaseCard } from '@/components/base'
import NewPasswordFields from '@/components/account/NewPasswordFields.vue'
import { useLinkToken } from '@/composables/useLinkToken'
import AuthFrame from '@/layouts/AuthFrame.vue'
import { newPasswordProblem } from '@/password'

const token = useLinkToken()
const password = ref('')
const confirm = ref('')
const submitting = ref(false)
const done = ref(false)
const error = ref<unknown>(null)
const tokenInvalid = ref(token === null)

const problem = computed(() => newPasswordProblem(password.value, confirm.value))
const passwordError = computed(() => (error.value instanceof ApiError ? error.value.field('password') : undefined))

async function submit() {
  if (!token || submitting.value || problem.value) return
  submitting.value = true
  error.value = null
  try {
    await authApi.completePasswordReset(token, password.value)
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
  <AuthFrame title="Choose a new password" :description="done || tokenInvalid ? undefined : 'Set the password you will use to sign in from now on.'">
    <BaseCard as="div" class="!gap-4 !p-6">
      <div v-if="done" role="status" class="flex flex-col gap-3">
        <p class="m-0">Your password has been changed. For safety, every device signed in to your account was signed out.</p>
        <BaseButton variant="primary" size="lg" block :to="{ name: 'sign-in' }">Sign in</BaseButton>
      </div>
      <div v-else-if="tokenInvalid" class="flex flex-col gap-3">
        <BaseAlert :error="token ? 'This reset link is invalid, has already been used, or has expired.' : 'This reset link is incomplete.'" />
        <p class="m-0 text-muted">Links work once and expire after 30 minutes. Requesting a new link cancels older ones.</p>
        <BaseButton variant="primary" size="lg" block :to="{ name: 'forgot-password' }">Request a new link</BaseButton>
        <RouterLink :to="{ name: 'sign-in' }" class="self-center text-[13px] text-muted hover:text-text hover:underline">Back to sign in</RouterLink>
      </div>
      <form v-else class="flex flex-col gap-4" novalidate @submit.prevent="submit">
        <BaseAlert v-if="error && !passwordError" :error="error" />
        <NewPasswordFields v-model="password" v-model:confirm="confirm" :error="passwordError" size="lg" />
        <BaseButton type="submit" variant="primary" size="lg" block :loading="submitting" :disabled="!!problem">
          {{ submitting ? 'Saving…' : 'Set new password' }}
        </BaseButton>
      </form>
    </BaseCard>
  </AuthFrame>
</template>
