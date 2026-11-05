<script setup lang="ts">
// Native <dialog> modal: focus containment, Esc and inert background come from the browser.
import { IconX } from '@tabler/icons-vue'
import { nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import IconButton from './IconButton.vue'

const props = withDefaults(
  defineProps<{
    title: string
    description?: string
    width?: 'sm' | 'md' | 'lg' | 'full'
    /**
     * Where focus goes on close when the element that opened the dialog no longer exists (a menu item, a
     * form that was replaced). Without it focus goes to the page's main region, never to the page body.
     */
    returnFocus?: () => HTMLElement | null | undefined
  }>(),
  { width: 'sm' },
)
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ close: [] }>()
const dialog = ref<HTMLDialogElement | null>(null)
const uid = useId()
let opener: HTMLElement | null = null

watch(
  open,
  async (v) => {
    await nextTick()
    const d = dialog.value
    if (!d) return
    if (v && !d.open) {
      opener = document.activeElement as HTMLElement | null
      d.showModal()
    } else if (!v && d.open) {
      d.close()
    }
  },
  { immediate: true },
)

function onClose() {
  if (open.value) open.value = false
  emit('close')
  restoreFocus()
}

/** Back to the opener; when it is gone, to `returnFocus` or the main region (keyboard users keep their place). */
function restoreFocus() {
  const from = opener
  opener = null
  if (from && from.isConnected && from !== document.body) {
    from.focus?.()
    return
  }
  const target = props.returnFocus?.() ?? mainRegion()
  target?.focus({ preventScroll: true })
}

function mainRegion(): HTMLElement | null {
  const main = document.getElementById('main')
  if (main && !main.hasAttribute('tabindex')) main.setAttribute('tabindex', '-1')
  return main
}
onBeforeUnmount(() => dialog.value?.open && dialog.value.close())
</script>

<template>
  <dialog
    ref="dialog"
    class="m-auto rounded-md border border-border bg-surface p-0 text-text shadow-xl backdrop:bg-backdrop"
    :class="width === 'full' ? 'h-[calc(100dvh-48px)] max-h-none w-[min(1100px,calc(100vw-32px))] max-w-none' : width === 'lg' ? 'w-[min(760px,calc(100vw-32px))]' : width === 'md' ? 'w-[min(560px,calc(100vw-32px))]' : 'w-[min(420px,calc(100vw-32px))]'"
    :aria-labelledby="`${uid}-title`"
    :aria-describedby="description ? `${uid}-desc` : undefined"
    @close="onClose"
  >
    <div v-if="open" class="flex flex-col gap-3 p-4">
      <div class="flex items-start gap-2">
        <div class="flex flex-1 flex-col gap-0.5">
          <h2 :id="`${uid}-title`" class="m-0 text-[15px] font-semibold">{{ title }}</h2>
          <p v-if="description" :id="`${uid}-desc`" class="m-0 text-muted">{{ description }}</p>
        </div>
        <IconButton label="Close dialog" @click="open = false"><IconX :size="16" stroke="1.75" /></IconButton>
      </div>
      <slot />
      <div v-if="$slots.footer" class="flex flex-wrap justify-end gap-2 pt-1"><slot name="footer" /></div>
    </div>
  </dialog>
</template>
