<script setup lang="ts">
// Search field with a leading icon and a visually hidden label. Extra attributes (ARIA combobox
// wiring, keydown handlers, maxlength) go to the <input>. `size="lg"` is the dialog variant.
import { IconSearch } from '@tabler/icons-vue'
import { ref, useId } from 'vue'
import KbdKey from './KbdKey.vue'
import Spinner from './Spinner.vue'

defineOptions({ inheritAttrs: false })
const props = withDefaults(
  defineProps<{ label: string; placeholder?: string; shortcut?: string; id?: string; size?: 'md' | 'lg'; loading?: boolean; type?: 'search' | 'text' }>(),
  { size: 'md', loading: false, type: 'search' },
)
const model = defineModel<string>({ default: '' })
const auto = useId()
const inputId = props.id ?? `search-${auto}`
const el = ref<HTMLInputElement | null>(null)
defineExpose({ focus: () => el.value?.focus(), select: () => el.value?.select() })
</script>

<template>
  <div class="relative">
    <label :for="inputId" class="sr-only">{{ label }}</label>
    <IconSearch :size="16" stroke="1.75" aria-hidden="true" class="absolute left-2.5 text-muted" :class="size === 'lg' ? 'top-[11px]' : 'top-[9px]'" />
    <input
      :id="inputId"
      ref="el"
      v-model="model"
      v-bind="$attrs"
      :type="type"
      :placeholder="placeholder"
      class="w-full rounded-md border border-border bg-bg pl-[34px]"
      :class="[size === 'lg' ? 'h-[38px] text-[14px]' : 'h-[34px] text-[13px]', shortcut || loading ? 'pr-11' : 'pr-2']"
    />
    <Spinner v-if="loading" :size="14" class="absolute right-3" :class="size === 'lg' ? 'top-3' : 'top-2.5'" />
    <KbdKey v-else-if="shortcut" aria-hidden="true" class="absolute top-2 right-2">{{ shortcut }}</KbdKey>
  </div>
</template>
