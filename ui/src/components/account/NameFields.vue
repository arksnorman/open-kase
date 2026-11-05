<script setup lang="ts">
// First (given), middle (optional) and last (family) name fields for a staff user. The display name
// shown everywhere is derived by the server: the parts joined with single spaces.
import type { NameParts } from '@/api/types'
import { FormField, TextInput } from '@/components/base'

const model = defineModel<NameParts>({ required: true })
withDefaults(
  defineProps<{
    /** Field error text by request field (`givenName`, `middleName`, `familyName`). */
    errors?: (field: keyof NameParts) => string | undefined
    /** All three optional (invitation acceptance: empty keeps the stored name). */
    allOptional?: boolean
    /** One column (narrow forms) instead of three side by side from the small breakpoint. */
    stacked?: boolean
    size?: 'sm' | 'md' | 'lg'
  }>(),
  { errors: () => undefined, allOptional: false, stacked: false, size: 'md' },
)
function set(field: keyof NameParts, v: string) {
  model.value = { ...model.value, [field]: v }
}
</script>

<template>
  <div class="grid grid-cols-1 gap-3" :class="stacked ? '' : 'sm:grid-cols-3'">
    <FormField label="First name / given name" :required="!allOptional" :optional="allOptional" :error="errors('givenName')">
      <TextInput :model-value="model.givenName" :size="size" autocomplete="given-name" maxlength="50" @update:model-value="(v: string) => set('givenName', v)" />
    </FormField>
    <FormField label="Middle name" optional :error="errors('middleName')">
      <TextInput :model-value="model.middleName" :size="size" autocomplete="additional-name" maxlength="50" @update:model-value="(v: string) => set('middleName', v)" />
    </FormField>
    <FormField label="Last name / family name" :required="!allOptional" :optional="allOptional" :error="errors('familyName')">
      <TextInput :model-value="model.familyName" :size="size" autocomplete="family-name" maxlength="50" @update:model-value="(v: string) => set('familyName', v)" />
    </FormField>
  </div>
</template>
