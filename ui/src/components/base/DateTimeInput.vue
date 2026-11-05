<script setup lang="ts">
// Date and time in the user's browser time zone (native datetime-local picker, keyboard
// accessible). The model is an ISO 8601 instant in UTC ("" when empty or incomplete), so callers
// never handle local wall-clock strings. `min` (an ISO instant) limits the picker.
import { computed, ref, watch } from 'vue'
import { useFieldControl } from './field'

const props = withDefaults(defineProps<{ id?: string; invalid?: boolean; min?: string; size?: 'sm' | 'md' }>(), { size: 'md' })
const model = defineModel<string>({ default: '' })
const { id, describedBy, invalid, required } = useFieldControl(props)

const pad = (n: number) => String(n).padStart(2, '0')
/** ISO instant → "YYYY-MM-DDTHH:mm" in local time. */
function toLocal(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}
/** "YYYY-MM-DDTHH:mm" (local) → ISO instant, or "" when incomplete. */
function toISO(local: string): string {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(local)) return ''
  const d = new Date(local) // a date-time without a zone is local time
  return Number.isNaN(d.getTime()) ? '' : d.toISOString()
}

const local = ref(toLocal(model.value))
watch(model, (v) => {
  if (toISO(local.value) !== v) local.value = v ? toLocal(v) : ''
})
function onInput(e: Event) {
  local.value = (e.target as HTMLInputElement).value
  model.value = toISO(local.value)
}
const minLocal = computed(() => (props.min ? toLocal(props.min) : undefined))
const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
</script>

<template>
  <span class="flex items-center gap-2">
    <input
      :id="id"
      type="datetime-local"
      class="lg-input w-auto"
      :class="size === 'sm' ? 'h-[34px]' : ''"
      :value="local"
      :min="minLocal"
      step="60"
      :aria-invalid="invalid ? 'true' : undefined"
      :aria-describedby="describedBy"
      :required="required || undefined"
      @input="onInput"
      @change="onInput"
    />
    <span class="text-xs text-muted">{{ zone }}</span>
  </span>
</template>
