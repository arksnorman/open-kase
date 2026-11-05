<script setup lang="ts">
// Password reset request. The API answers 202 for every address (unknown, disabled or invited
// included), so the confirmation is the same whatever was entered.
import { ref } from 'vue'
import { authApi } from '@/api/endpoints'
import { ApiError, retryHint } from '@/api/errors'
import { BaseAlert, BaseButton, BaseCard, FormField, TextInput } from '@/components/base'
import AuthFrame from '@/layouts/AuthFrame.vue'

const email = ref('')
const submitting = ref(false)
const sent = ref(false)
const error = ref<unknown>(null)

const fieldError = () => (error.value instanceof ApiError ? error.value.field('email') : undefined)

async function submit() {
  if (submitting.value || !email.value.trim()) return
  submitting.value = true
  error.value = null
  try {
    await authApi.requestPasswordReset(email.value.trim())
    sent.value = true
  } catch (err) {
    // Throttling counts every request (not only existing accounts), so saying so reveals nothing.
    error.value =
      err instanceof ApiError && err.status === 429
        ? `Too many reset requests. ${retryHint(err, 'Wait an hour before requesting another link.')}`
        : err
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AuthFrame title="Reset your password" :description="sent ? undefined : 'Enter the email address you sign in with. We will send a link to choose a new password.'">
    <BaseCard as="div" class="!gap-4 !p-6">
      <div v-if="sent" role="status" class="flex flex-col gap-3">
        <p class="m-0">If an active account uses that address, a reset link is on its way. The link works once and expires in 30 minutes.</p>
        <p class="m-0 text-muted">No email? Check spam, or ask your workspace administrator: email may not be configured for this installation.</p>
        <BaseButton variant="primary" size="lg" block :to="{ name: 'sign-in' }">Back to sign in</BaseButton>
      </div>
      <form v-else class="flex flex-col gap-4" novalidate @submit.prevent="submit">
        <BaseAlert v-if="error && !fieldError()" :error="error" />
        <FormField label="Email" :error="fieldError()" required>
          <TextInput v-model="email" type="email" name="email" autocomplete="username" size="lg" />
        </FormField>
        <BaseButton type="submit" variant="primary" size="lg" block :loading="submitting" :disabled="!email.trim()">
          {{ submitting ? 'Sending…' : 'Send reset link' }}
        </BaseButton>
        <RouterLink :to="{ name: 'sign-in' }" class="self-center text-[13px] text-muted hover:text-text hover:underline">Back to sign in</RouterLink>
      </form>
    </BaseCard>
  </AuthFrame>
</template>
