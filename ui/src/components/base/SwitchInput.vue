<script setup lang="ts">
// On/off switch: a button with role="switch" and aria-checked, indigo when on. The visible label
// and any description live outside (pass their ids as `labelledby` / `describedby`), or give
// `label` for an accessible name without visible text.
defineProps<{ label?: string; labelledby?: string; describedby?: string; disabled?: boolean; id?: string }>()
const model = defineModel<boolean>({ default: false })
</script>

<template>
  <button
    :id="id"
    type="button"
    role="switch"
    :aria-checked="model ? 'true' : 'false'"
    :aria-label="labelledby ? undefined : label"
    :aria-labelledby="labelledby"
    :aria-describedby="describedby"
    :disabled="disabled"
    class="relative inline-flex h-[22px] w-10 flex-none cursor-pointer items-center rounded-full border-0 p-0 transition-colors duration-150 disabled:cursor-not-allowed disabled:opacity-50 motion-reduce:transition-none"
    :class="model ? 'bg-accent' : 'bg-border-strong'"
    @click="model = !model"
  >
    <span
      aria-hidden="true"
      class="absolute top-[3px] size-4 rounded-full bg-white shadow-sm transition-[left] duration-150 motion-reduce:transition-none"
      :class="model ? 'left-[21px]' : 'left-[3px]'"
    ></span>
  </button>
</template>
