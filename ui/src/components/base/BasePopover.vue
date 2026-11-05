<script setup lang="ts">
// Anchored, non-modal popover: outside click and Esc close it and return focus to the trigger.
// Positioned with floating.ts (flips above the trigger near the bottom of the viewport, stays
// on screen, renders in the top layer so it works inside BaseModal and scroll containers).
import type { Placement } from '@floating-ui/dom'
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import { supportsPopover, useFloating } from './floating'

const props = withDefaults(defineProps<{ placement?: 'bottom-end' | 'bottom-start' | 'top-start' | 'bottom-stretch' | 'right-end'; panelClass?: string }>(), {
  placement: 'bottom-end',
})
const open = defineModel<boolean>('open', { default: false })
const root = ref<HTMLElement | null>(null)
const triggerWrap = ref<HTMLElement | null>(null)
const panel = ref<HTMLElement | null>(null)
const panelId = useId()

const anchor = computed(() => (triggerWrap.value?.firstElementChild as HTMLElement | null) ?? triggerWrap.value)
const { style } = useFloating(anchor, panel, open, {
  placement: () => (props.placement === 'bottom-stretch' ? 'bottom-start' : (props.placement as Placement)),
  matchWidth: () => (props.placement === 'bottom-stretch' ? 'exact' : false),
})

function onOutside(e: PointerEvent) {
  const t = e.target as Node | null
  // A node detached by the click being dispatched (a removed chip) was inside when pressed.
  if (!t || !t.isConnected) return
  if (root.value && !root.value.contains(t)) close(false)
}
function close(returnFocus = true) {
  open.value = false
  if (returnFocus) triggerWrap.value?.querySelector<HTMLElement>('button, [tabindex]')?.focus({ preventScroll: true })
}
// Escape closes the innermost open thing only: preventDefault stops a surrounding native
// <dialog> from treating the same key press as its own close request.
function onEsc(e: KeyboardEvent) {
  if (!open.value) return
  e.preventDefault()
  e.stopPropagation()
  close()
}
// Tabbing out of the panel (or trigger) to elsewhere closes it, as menus and popovers do.
function onFocusOut(e: FocusEvent) {
  const next = e.relatedTarget as Node | null
  if (open.value && next && root.value && !root.value.contains(next)) close(false)
}
watch(open, async (v) => {
  if (v) {
    document.addEventListener('pointerdown', onOutside, true)
    await nextTick()
    const p = panel.value
    if (!p) return
    // useFloating shows the [popover] panel in the top layer, possibly after this runs; a hidden
    // popover cannot take focus (the menu then stayed unreachable by arrow keys), so show it first.
    if (supportsPopover && p.hasAttribute('popover') && !p.matches(':popover-open')) {
      try {
        p.showPopover()
      } catch {
        /* detached or already shown */
      }
    }
    p.querySelector<HTMLElement>(
      '[autofocus], input:not([disabled]), [role="menuitem"]:not([disabled]), [role="radio"][tabindex="0"], a[href], button:not([disabled]), select:not([disabled])',
    )?.focus({ preventScroll: true })
  } else {
    document.removeEventListener('pointerdown', onOutside, true)
  }
})
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOutside, true))
defineExpose({ close })
</script>

<template>
  <div ref="root" class="relative" @keydown.esc="onEsc" @focusout="onFocusOut">
    <div ref="triggerWrap" data-popover-trigger>
      <slot name="trigger" :open="open" :toggle="() => (open = !open)" :panel-id="panelId" />
    </div>
    <div v-if="open" :id="panelId" ref="panel" class="lg-float" :popover="supportsPopover ? 'manual' : undefined" :style="style" :class="panelClass">
      <slot :close="close" />
    </div>
  </div>
</template>
