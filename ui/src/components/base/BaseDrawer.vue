<script setup lang="ts">
// Off-canvas panel on a native modal <dialog>: the browser makes the page behind it inert (focus
// stays inside) and Esc closes it. This adds focus return to the opener, closing on a scrim
// click and a page scroll lock. Content stays mounted while closed, so state inside it (a
// selected tab, a loaded list) survives; consumers render their own close button with the
// slot's `close`. Used by the shell navigation below desktop width and by the ticket side
// panel on narrow workspaces.
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

withDefaults(defineProps<{ label: string; side?: 'left' | 'right' }>(), { side: 'left' })
const open = defineModel<boolean>('open', { default: false })
const dialog = ref<HTMLDialogElement | null>(null)
let opener: HTMLElement | null = null
let savedOverflow: string | null = null

function lockScroll(on: boolean) {
  const root = document.documentElement
  if (on && savedOverflow === null) {
    savedOverflow = root.style.overflow
    root.style.overflow = 'hidden'
  } else if (!on && savedOverflow !== null) {
    root.style.overflow = savedOverflow
    savedOverflow = null
  }
}

function sync(v: boolean) {
  const d = dialog.value
  if (!d) return
  if (v && !d.open) {
    opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
    d.showModal()
    lockScroll(true)
  } else if (!v && d.open) {
    d.close()
  }
}
// Applied in the same flush as the change (callers can `await nextTick()` and find the drawer
// closed and focus back on the opener); before mount, once the dialog exists.
watch(
  open,
  (v) => {
    if (dialog.value) sync(v)
    else void nextTick(() => sync(open.value))
  },
  { immediate: true },
)

function onClose() {
  lockScroll(false)
  if (open.value) open.value = false
  const target = opener
  opener = null
  if (target?.isConnected) target.focus({ preventScroll: true })
}

// The panel content fills the dialog box, so a click whose target is the dialog itself landed
// on the scrim (::backdrop) beside the panel.
function onClick(e: MouseEvent) {
  if (e.target === dialog.value) open.value = false
}

function close() {
  open.value = false
}

onBeforeUnmount(() => {
  lockScroll(false)
  if (dialog.value?.open) dialog.value.close()
})
</script>

<template>
  <dialog ref="dialog" class="lg-drawer" :data-side="side" :aria-label="label" @close="onClose" @click="onClick">
    <div class="flex h-full min-h-0 flex-col">
      <slot :close="close" />
    </div>
  </dialog>
</template>
