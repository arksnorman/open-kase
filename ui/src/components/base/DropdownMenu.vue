<script setup lang="ts">
// Action menu (role=menu) with arrow-key navigation.
import { ref } from 'vue'
import BasePopover from './BasePopover.vue'
import type { MenuItem } from './types'

defineProps<{ items: MenuItem[]; label: string; emptyText?: string }>()
const open = ref(false)
const menu = ref<HTMLElement | null>(null)

function onKey(e: KeyboardEvent) {
  const items = Array.from(menu.value?.querySelectorAll<HTMLElement>('[role="menuitem"]:not([disabled])') ?? [])
  const i = items.indexOf(document.activeElement as HTMLElement)
  if (e.key === 'ArrowDown') items[(i + 1) % items.length]?.focus()
  else if (e.key === 'ArrowUp') items[(i - 1 + items.length) % items.length]?.focus()
  else if (e.key === 'Home') items[0]?.focus()
  else if (e.key === 'End') items[items.length - 1]?.focus()
  else return
  e.preventDefault()
}
function run(item: MenuItem, close: (f?: boolean) => void) {
  close(true)
  item.action()
}
</script>

<template>
  <BasePopover v-model:open="open" panel-class="w-52 p-1">
    <template #trigger="slotProps">
      <slot name="trigger" v-bind="slotProps" />
    </template>
    <template #default="{ close }">
      <div ref="menu" role="menu" :aria-label="label" class="flex flex-col" @keydown="onKey">
        <button
          v-for="item in items"
          :key="item.label"
          type="button"
          role="menuitem"
          class="lg-nav lg-hov w-full border-0 bg-transparent text-left"
          :class="item.tone === 'danger' ? 'text-danger' : ''"
          :disabled="item.disabled"
          @click="run(item, close)"
        >
          {{ item.label }}
        </button>
        <span v-if="!items.length" class="px-2 py-1.5 text-muted">{{ emptyText ?? 'No actions available' }}</span>
      </div>
    </template>
  </BasePopover>
</template>
