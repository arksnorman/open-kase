<script setup lang="ts">
// Duration in whole minutes as "number + unit" (min or h). Empty = null; a value that is not a
// positive number emits NaN so the caller can flag it.
import { computed, ref, useId, watch } from 'vue'
import SelectInput from './SelectInput.vue'

const props = defineProps<{ label: string; disabled?: boolean; invalid?: boolean }>()
const model = defineModel<number | null>({ default: null })
const uid = useId()

const UNITS: { value: number; label: string }[] = [
  { value: 60, label: 'h' },
  { value: 1, label: 'min' },
]

function split(m: number | null): { amount: string; unit: number } {
  if (m === null) return { amount: '', unit: 60 }
  for (const u of UNITS) if (m % u.value === 0 && m >= u.value) return { amount: String(m / u.value), unit: u.value }
  return { amount: String(m), unit: 1 }
}

// Vue's v-model gives a number for type="number" inputs once the user types; keep it as text.
const amount = ref<string | number>('')
const amountText = () => String(amount.value ?? '').trim()
const unit = ref(60)
watch(
  model,
  (m) => {
    // Keep the agent's chosen unit when the value is equivalent.
    const cur = amountText() === '' ? null : Math.round(Number(amountText()) * unit.value)
    if (cur === m) return
    const s = split(m)
    amount.value = s.amount
    unit.value = s.unit
  },
  { immediate: true },
)

function emitValue() {
  const raw = amountText()
  if (raw === '') {
    model.value = null
    return
  }
  const n = Number(raw)
  model.value = Number.isFinite(n) && n > 0 ? Math.round(n * unit.value) : NaN
}
const invalidLocal = computed(() => props.invalid || (model.value !== null && !Number.isFinite(model.value)))
</script>

<template>
  <div class="flex items-center gap-1" role="group" :aria-label="label">
    <label :for="`${uid}-n`" class="sr-only">{{ label }} amount</label>
    <input
      :id="`${uid}-n`"
      v-model="amount"
      type="number"
      min="0"
      step="any"
      inputmode="decimal"
      class="lg-input h-[34px] w-[72px] !px-2"
      :disabled="disabled"
      :aria-invalid="invalidLocal ? 'true' : undefined"
      placeholder="—"
      @input="emitValue"
    />
    <SelectInput
      :id="`${uid}-u`"
      :model-value="unit"
      :options="UNITS"
      :aria-label="`${label} unit`"
      :disabled="disabled"
      size="sm"
      class="w-[76px]"
      @update:model-value="(v) => { if (typeof v === 'number') { unit = v; emitValue() } }"
    />
  </div>
</template>
