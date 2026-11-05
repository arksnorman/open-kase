<script setup lang="ts">
// Account preferences: theme (applied immediately) and optional email notices.
import { ref, watch } from 'vue'
import { meApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import { BaseAlert, BaseButton, BaseModal, CheckboxInput, FormField, TextArea } from '@/components/base'
import { useSessionStore } from '@/stores/session'
import ThemeSwitcher from './ThemeSwitcher.vue'

const open = defineModel<boolean>('open', { default: false })
const session = useSessionStore()

const watcherUpdates = ref(true)
const queueUpdates = ref(true)
const signature = ref('')
const sigBusy = ref(false)
const sigError = ref<unknown>(null)
const sigSaved = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)
const saved = ref(false)

watch(
  open,
  (v) => {
    if (!v) return
    const prefs = session.user?.emailPreferences
    watcherUpdates.value = prefs?.watcherUpdates ?? true
    queueUpdates.value = prefs?.queueUpdates ?? true
    signature.value = session.user?.signature ?? ''
    error.value = null
    saved.value = false
    sigError.value = null
    sigSaved.value = false
  },
  { immediate: true },
)

async function save() {
  busy.value = true
  error.value = null
  saved.value = false
  try {
    const user = await meApi.setEmailPreferences({ watcherUpdates: watcherUpdates.value, queueUpdates: queueUpdates.value })
    session.setUser(user)
    saved.value = true
  } catch (err) {
    error.value = err
  } finally {
    busy.value = false
  }
}

async function saveSignature() {
  sigBusy.value = true
  sigError.value = null
  sigSaved.value = false
  try {
    session.setUser(await meApi.setSignature(signature.value.trim()))
    sigSaved.value = true
  } catch (err) {
    sigError.value = err
  } finally {
    sigBusy.value = false
  }
}
</script>

<template>
  <BaseModal v-model:open="open" title="Preferences" description="Settings for your account on every device.">
    <div class="flex flex-col gap-4">
      <div class="flex flex-col gap-1.5">
        <span class="lg-label">Theme</span>
        <ThemeSwitcher />
        <span class="text-xs text-muted">Applied immediately.</span>
      </div>
      <form class="flex flex-col gap-3 border-t border-line pt-3" novalidate @submit.prevent="saveSignature">
        <FormField label="Reply signature" optional hint="Markdown, up to 1000 characters. Added automatically after your public replies (never to notes); you will not see it in the reply box. You can switch it off per reply." :error="sigError instanceof ApiError ? sigError.field('signature') : undefined">
          <TextArea v-model="signature" :rows="3" />
        </FormField>
        <BaseAlert v-if="sigError && !(sigError instanceof ApiError && sigError.fieldErrors.length)" :error="sigError" />
        <div class="flex items-center gap-3">
          <BaseButton type="submit" :loading="sigBusy">Save signature</BaseButton>
          <span v-if="sigSaved" role="status" class="text-xs text-muted">Signature saved.</span>
        </div>
      </form>
      <form id="prefs-email" class="flex flex-col gap-3 border-t border-line pt-3" novalidate @submit.prevent="save">
        <span class="lg-label">Email notifications</span>
        <CheckboxInput v-model="watcherUpdates" label="Updates on tickets I watch" hint="Replies, notes and status changes on tickets you watch but are not assigned to." />
        <CheckboxInput v-model="queueUpdates" label="New and unassigned tickets in my groups" hint="Notices when a ticket enters one of your groups' queues unassigned." />
        <p class="m-0 text-xs text-muted">Emails about tickets assigned to you are always sent and cannot be muted. Email is only sent when an administrator has configured it.</p>
        <BaseAlert v-if="error" :error="error" />
        <span v-if="saved" role="status" class="text-xs text-muted">Email preferences saved.</span>
      </form>
    </div>
    <template #footer>
      <BaseButton @click="open = false">Close</BaseButton>
      <BaseButton type="submit" form="prefs-email" variant="primary" :loading="busy">Save email preferences</BaseButton>
    </template>
  </BaseModal>
</template>
