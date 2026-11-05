<script setup lang="ts">
// New password + confirmation with a local strength hint. The parent submits; `error` is the
// server's message for the password field (e.g. from a 422).
import { computed } from 'vue'
import { FormField, PasswordInput } from '@/components/base'
import { passwordStrength } from '@/password'

withDefaults(defineProps<{ label?: string; error?: string; size?: 'md' | 'lg' }>(), { label: 'New password', size: 'md' })
const password = defineModel<string>({ default: '' })
const confirm = defineModel<string>('confirm', { default: '' })

const strength = computed(() => passwordStrength(password.value))
const mismatch = computed(() => (confirm.value !== '' && confirm.value !== password.value ? 'The passwords do not match.' : undefined))
</script>

<template>
  <FormField :label="label" :error="error" required>
    <PasswordInput v-model="password" name="new-password" autocomplete="new-password" :size="size" maxlength="256" />
    <div v-if="password" class="flex flex-col gap-1" aria-live="polite">
      <div class="flex gap-1" aria-hidden="true">
        <span
          v-for="i in 4"
          :key="i"
          class="h-1 flex-1 rounded-full"
          :class="i <= strength.level ? (strength.level <= 1 ? 'bg-warn' : 'bg-text') : 'bg-line'"
        ></span>
      </div>
      <span class="text-xs text-muted">
        <span class="font-medium" :class="strength.level <= 1 ? 'text-warn' : 'text-text'">{{ strength.label }}.</span>
        {{ strength.hint }}
      </span>
    </div>
    <span v-else class="text-xs text-muted">At least 12 characters.</span>
  </FormField>
  <FormField :label="`Confirm ${label.toLowerCase()}`" :error="mismatch" required>
    <PasswordInput v-model="confirm" name="confirm-password" autocomplete="new-password" :size="size" maxlength="256" />
  </FormField>
</template>
