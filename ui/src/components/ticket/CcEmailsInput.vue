<script setup lang="ts">
// CC addresses editor (Recipients dialog, new ticket form): type one or more addresses (comma, space
// or semicolon separated) and press Enter or Add; each becomes a removable chip. CC addresses get
// every email the requester gets (never message bodies). `commit()` adds pending typed text and
// reports whether it was valid, so a form can flush it before saving.
import { IconPlus } from '@tabler/icons-vue'
import { ref } from 'vue'
import { BaseAvatar, BaseBadge, BaseButton, FormField, TextInput } from '@/components/base'

const MAX = 10

const props = defineProps<{ requesterEmail?: string; disabled?: boolean; error?: string; label?: string; hint?: string }>()
const model = defineModel<string[]>({ required: true })

const input = ref('')
const inputError = ref<string | null>(null)

/** Adds the typed address(es); false when one is invalid. */
function commit(): boolean {
  inputError.value = null
  const parts = input.value.split(/[\s,;]+/).map((s) => s.trim().toLowerCase()).filter(Boolean)
  const next = [...model.value]
  for (const a of parts) {
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(a)) {
      inputError.value = `“${a}” is not a valid email address.`
      return false
    }
    if (props.requesterEmail && a === props.requesterEmail.trim().toLowerCase()) {
      inputError.value = 'The requester already receives these emails.'
      return false
    }
    if (!next.includes(a)) {
      if (next.length >= MAX) {
        inputError.value = `A ticket can have at most ${MAX} CC addresses.`
        return false
      }
      next.push(a)
    }
  }
  model.value = next
  input.value = ''
  return true
}

function remove(a: string) {
  model.value = model.value.filter((x) => x !== a)
}

/** Typed text that has not been added yet. */
const pending = () => !!input.value.trim()

defineExpose({ commit, pending, reset: () => { input.value = ''; inputError.value = null } })
</script>

<template>
  <div class="flex flex-col gap-2">
    <FormField :label="label ?? 'CC addresses'" optional :error="inputError ?? error" :hint="hint ?? `Up to ${MAX}. Press Enter to add. They get the same email notices as the requester, and a notice when the requester replies or resolves the ticket.`">
      <div class="flex gap-2">
        <TextInput v-model="input" type="email" size="sm" placeholder="name@example.com" :disabled="disabled" @keydown.enter.prevent="commit()" />
        <BaseButton size="sm" :disabled="disabled || !input.trim()" @click="commit()">
          <template #icon><IconPlus :size="14" stroke="1.75" aria-hidden="true" /></template>
          Add
        </BaseButton>
      </div>
    </FormField>
    <ul v-if="model.length" class="m-0 flex list-none flex-wrap gap-1.5 p-0" aria-label="CC addresses">
      <li v-for="a in model" :key="a">
        <BaseBadge removable :remove-label="`Remove ${a}`" :remove-disabled="disabled" class="!text-text" @remove="remove(a)">
          <BaseAvatar :name="a" role="email" size="2xs" decorative />{{ a }}
        </BaseBadge>
      </li>
    </ul>
  </div>
</template>
