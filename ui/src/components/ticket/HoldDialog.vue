<script setup lang="ts">
// "Put on hold": pick why (the reason fixes how long: it shows its maximum days) and add an
// optional internal note. The ticket returns to Open by itself when the time is up, or sooner when
// someone changes its status or the requester replies. The requester is not told.
import { computed, ref, watch } from 'vue'
import { holdReasonsApi } from '@/api/endpoints'
import { BaseAlert, BaseButton, BaseModal, FormField, SelectInput, TextArea } from '@/components/base'
import { useQuery } from '@/composables/useQuery'

const props = defineProps<{ busy?: boolean; error?: unknown }>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ submit: [value: { holdReasonId: string; note: string }] }>()

const reasonsQ = useQuery(
  () => (open.value ? 'hold-reasons' : null),
  (signal) => holdReasonsApi.list({ signal }),
)
const reasons = computed(() => reasonsQ.data.value?.items ?? [])
const options = computed(() =>
  reasons.value.map((r) => ({ value: r.id, label: r.name, hint: `${r.maxDays} day${r.maxDays === 1 ? '' : 's'}` })),
)
const reasonId = ref('')
const note = ref('')
const chosen = computed(() => reasons.value.find((r) => r.id === reasonId.value))

watch(open, (v) => {
  if (!v) return
  reasonId.value = ''
  note.value = ''
})

const submit = () => {
  if (reasonId.value) emit('submit', { holdReasonId: reasonId.value, note: note.value.trim() })
}
</script>

<template>
  <BaseModal
    v-model:open="open"
    title="Put on hold"
    description="The ticket leaves your working queues and returns to Open on its own when the hold ends. The requester is not notified."
  >
    <form class="flex flex-col gap-3" novalidate @submit.prevent="submit">
      <BaseAlert v-if="error" :error="error" />
      <BaseAlert v-else-if="reasonsQ.error.value" :error="reasonsQ.error.value" />
      <FormField
        label="Reason"
        required
        :hint="chosen ? `On hold for ${chosen.maxDays} day${chosen.maxDays === 1 ? '' : 's'}, then back to Open.` : 'Each reason sets how long the ticket stays on hold.'"
      >
        <SelectInput v-model="reasonId" :options="options" :loading="reasonsQ.loading.value" searchable search-placeholder="Search reasons" placeholder="Choose a reason" empty-text="No hold reasons are set up. An admin can add them in Settings." />
      </FormField>
      <FormField label="Note" optional hint="Saved as an internal note; the requester never sees it.">
        <TextArea v-model="note" :rows="3" />
      </FormField>
      <div class="flex justify-end gap-2">
        <BaseButton @click="open = false">Cancel</BaseButton>
        <BaseButton type="submit" variant="primary" :loading="busy" :disabled="!reasonId">Put on hold</BaseButton>
      </div>
    </form>
  </BaseModal>
</template>
