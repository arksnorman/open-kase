<script setup lang="ts" generic="T extends string">
// WAI-ARIA tabs. Panels are named slots matching each tab id.
import { ref, useId } from 'vue'
import type { TabDef } from './types'

const props = withDefaults(defineProps<{ tabs: TabDef<T>[]; label: string; variant?: 'rail' | 'compact' }>(), { variant: 'rail' })
const model = defineModel<T>({ required: true })
const uid = useId()
const els = ref<HTMLButtonElement[]>([])

function onKey(e: KeyboardEvent, i: number) {
  const n = props.tabs.length
  let next = -1
  if (e.key === 'ArrowRight') next = (i + 1) % n
  else if (e.key === 'ArrowLeft') next = (i - 1 + n) % n
  else if (e.key === 'Home') next = 0
  else if (e.key === 'End') next = n - 1
  if (next < 0) return
  e.preventDefault()
  const tab = props.tabs[next]
  if (tab) model.value = tab.id
  els.value[next]?.focus()
}
</script>

<template>
  <div class="flex min-h-0 flex-1 flex-col">
    <div class="flex flex-none items-end gap-1 border-b border-border" :class="variant === 'rail' ? 'h-[52px] px-4' : 'h-9 px-0'">
      <div role="tablist" :aria-label="label" class="flex items-end gap-1 self-stretch">
        <button
          v-for="(t, i) in tabs"
          :id="`${uid}-tab-${t.id}`"
          :key="t.id"
          ref="els"
          type="button"
          role="tab"
          class="lg-tab"
          :class="variant === 'compact' ? '!h-9' : ''"
          :aria-selected="model === t.id"
          :aria-controls="`${uid}-panel-${t.id}`"
          :tabindex="model === t.id ? 0 : -1"
          @click="model = t.id"
          @keydown="onKey($event, i)"
        >
          <component :is="t.icon" v-if="t.icon" :size="15" stroke="1.75" aria-hidden="true" class="mr-1 inline-block align-[-2px]" />{{ t.label }}
        </button>
      </div>
      <span class="flex-1"></span>
      <!-- Outside the tablist: only tabs may be its children. -->
      <slot name="extra" />
    </div>
    <div
      v-for="t in tabs"
      v-show="model === t.id"
      :id="`${uid}-panel-${t.id}`"
      :key="t.id"
      role="tabpanel"
      :aria-labelledby="`${uid}-tab-${t.id}`"
      class="flex min-h-0 flex-1 flex-col"
    >
      <slot :name="t.id" />
    </div>
  </div>
</template>
