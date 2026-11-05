<script setup lang="ts">
// Re-enter the current password before a sensitive account change. With `withCode` the dialog also asks
// for a current authenticator code or an unused recovery code. `action` receives the password and the
// code (6 digits = TOTP, anything else = recovery code). A 422 on `password` or the code stays in the
// dialog, anything else is shown above it.
import { ref, useId, watch } from 'vue'
import { ApiError, retryHint } from '@/api/errors'
import { BaseAlert, BaseButton, BaseModal, FormField, PasswordInput, TextInput } from '@/components/base'

const props = withDefaults(
  defineProps<{ title: string; description?: string; confirmLabel: string; danger?: boolean; withCode?: boolean; action: (password: string, code: { code?: string; recoveryCode?: string }) => Promise<void> }>(),
  { danger: false, withCode: false },
)
const open = defineModel<boolean>('open', { default: false })
const formId = `pw-confirm-${useId()}`
const password = ref('')
const codeText = ref('')
const busy = ref(false)
const error = ref<unknown>(null)

watch(open, (v) => {
  if (!v) {
    password.value = ''
    codeText.value = ''
    error.value = null
  }
})

const fieldError = () => {
  const e = error.value
  if (!(e instanceof ApiError)) return undefined
  return e.field('password') ? 'That password is not correct.' : undefined
}
const codeError = () => {
  const e = error.value
  if (!(e instanceof ApiError)) return undefined
  return e.field('code') || e.field('recoveryCode') ? 'That code is not correct or was already used.' : undefined
}
const formError = () => {
  const e = error.value
  if (!e || fieldError() || codeError()) return null
  if (e instanceof ApiError && e.status === 429) return `Too many attempts. ${retryHint(e, 'Wait a few minutes and try again.')}`
  return e
}

const canSubmit = () => !!password.value && (!props.withCode || !!codeText.value.trim())

async function submit() {
  if (!canSubmit() || busy.value) return
  busy.value = true
  error.value = null
  try {
    const raw = codeText.value.replace(/\s/g, '')
    await props.action(password.value, !props.withCode ? {} : /^\d{6}$/.test(raw) ? { code: raw } : { recoveryCode: raw })
    open.value = false
  } catch (err) {
    error.value = err
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <BaseModal v-model:open="open" :title="title" :description="description">
    <form :id="formId" class="flex flex-col gap-3" novalidate @submit.prevent="submit">
      <BaseAlert v-if="formError()" :error="formError()" />
      <FormField label="Current password" :error="fieldError()" required>
        <PasswordInput v-model="password" autocomplete="current-password" autofocus />
      </FormField>
      <FormField v-if="withCode" label="Authenticator code or recovery code" :error="codeError()" hint="The 6-digit code from your authenticator app, or one unused recovery code." required>
        <TextInput v-model="codeText" autocomplete="one-time-code" inputmode="text" />
      </FormField>
    </form>
    <template #footer>
      <BaseButton @click="open = false">Cancel</BaseButton>
      <BaseButton type="submit" :form="formId" :variant="danger ? 'danger' : 'primary'" :loading="busy" :disabled="!canSubmit()">
        {{ confirmLabel }}
      </BaseButton>
    </template>
  </BaseModal>
</template>
