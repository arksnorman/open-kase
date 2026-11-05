<script setup lang="ts">
// Frame shared by the signed-in shells (workspace, administration, account).
// From 1024 px: the Ledger sidebar (brand, the shell's navigation, the user menu) beside the main
// area. It collapses to an icon rail and can be hidden completely (a slim strip keeps the reopen
// button); `[` switches between the full sidebar and the rail. The choice is remembered per browser.
// Narrower: a top bar (navigation button, the shell's title and actions, the user menu) with the
// navigation in a drawer (`[` opens and closes it), so queues, tables and forms get the full width
// on tablets and phones. The main area keeps its identity across the switch (the routed view is
// not remounted).
import { IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand, IconMenu2, IconX } from '@tabler/icons-vue'
import { nextTick, onBeforeUnmount, onMounted, ref, useId, watch } from 'vue'
import { useRoute } from 'vue-router'
import BrandMark from '@/components/BrandMark.vue'
import UserMenu from '@/components/UserMenu.vue'
import { BaseDrawer, IconButton, SkipLink } from '@/components/base'
import { DESKTOP_SHELL_QUERY, useMediaQuery } from '@/composables/useMediaQuery'
import { isTypingTarget, readStored, writeStored } from '@/panels'

defineProps<{
  /** Accessible name of the navigation (sidebar or drawer). */
  navLabel: string
  /** Extra classes for <main> (e.g. `overflow-auto` for pages that scroll as a whole). */
  mainClass?: string
}>()

type NavMode = 'open' | 'rail' | 'hidden'
const NAV_KEY = 'oc.nav'

const desktop = useMediaQuery(DESKTOP_SHELL_QUERY)
const drawerOpen = ref(false)
const route = useRoute()
const navId = `shell-nav-${useId()}`

const stored = readStored(NAV_KEY)
const mode = ref<NavMode>(stored === 'rail' || stored === 'hidden' ? stored : 'open')
watch(mode, async (v) => {
  writeStored(NAV_KEY, v)
  // The button that was pressed is replaced by the other layout's toggle: keep keyboard focus there.
  await nextTick()
  const lost = document.activeElement === null || document.activeElement === document.body
  if (lost) document.querySelector<HTMLElement>('[data-nav-toggle]')?.focus({ preventScroll: true })
})

// Following a link in the drawer (or any navigation) closes it; so does widening the window.
watch(
  () => route.fullPath,
  () => {
    drawerOpen.value = false
  },
)
watch(desktop, (v) => {
  if (v) drawerOpen.value = false
})

function toggleNav() {
  if (!desktop.value) drawerOpen.value = !drawerOpen.value
  else mode.value = mode.value === 'open' ? 'rail' : 'open'
}

function onKey(e: KeyboardEvent) {
  if (e.key !== '[' || e.metaKey || e.ctrlKey || e.altKey || e.repeat || isTypingTarget(e.target)) return
  // A modal (search, a form) is open: the shortcut belongs to it, not the page behind. The nav
  // drawer itself is a modal too, so `[` still closes it.
  if (document.querySelector('dialog[open]') && !drawerOpen.value) return
  e.preventDefault()
  toggleNav()
}
onMounted(() => document.addEventListener('keydown', onKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <div class="flex h-dvh min-h-0 bg-bg text-text" :class="desktop ? 'flex-row' : 'flex-col'">
    <SkipLink />
    <template v-if="desktop">
      <aside v-if="mode === 'open'" :id="navId" :aria-label="navLabel" class="flex w-60 flex-none flex-col border-r border-border bg-surface">
        <div class="flex h-[52px] flex-none items-center justify-between gap-2 pr-2 pl-4">
          <BrandMark />
          <IconButton label="Collapse sidebar ([)" :expanded="true" :aria-controls="navId" aria-keyshortcuts="[" data-nav-toggle @click="mode = 'rail'">
            <IconLayoutSidebarLeftCollapse :size="18" stroke="1.75" />
          </IconButton>
        </div>
        <slot name="nav" :compact="false" :rail="false" />
        <UserMenu />
      </aside>
      <aside v-else-if="mode === 'rail'" :id="navId" :aria-label="navLabel" class="lg-rail flex w-14 flex-none flex-col items-center border-r border-border bg-surface">
        <div class="flex h-[52px] flex-none items-center justify-center">
          <IconButton label="Expand sidebar ([)" :expanded="false" :aria-controls="navId" aria-keyshortcuts="[" data-nav-toggle @click="mode = 'open'">
            <IconLayoutSidebarLeftExpand :size="18" stroke="1.75" />
          </IconButton>
        </div>
        <slot name="nav" :compact="false" :rail="true" />
        <div class="flex flex-none flex-col items-center gap-1 border-t border-line py-2">
          <IconButton label="Hide sidebar" @click="mode = 'hidden'">
            <IconX :size="16" stroke="1.75" />
          </IconButton>
          <UserMenu compact />
        </div>
      </aside>
      <div v-else class="flex w-10 flex-none flex-col items-center border-r border-border bg-surface pt-2.5">
        <IconButton label="Show sidebar ([)" :expanded="false" aria-keyshortcuts="[" data-nav-toggle @click="mode = 'open'">
          <IconLayoutSidebarLeftExpand :size="18" stroke="1.75" />
        </IconButton>
      </div>
    </template>
    <header v-else class="flex h-12 flex-none items-center gap-1.5 border-b border-border bg-surface px-2">
      <IconButton label="Open navigation ([)" aria-haspopup="dialog" aria-keyshortcuts="[" :expanded="drawerOpen" @click="drawerOpen = true">
        <IconMenu2 :size="18" stroke="1.75" />
      </IconButton>
      <div class="flex min-w-0 flex-1 items-center gap-2">
        <slot name="title"><BrandMark /></slot>
      </div>
      <slot name="actions" />
      <UserMenu compact />
    </header>

    <!-- tabindex: the skip link and dialogs whose opener is gone move focus here. -->
    <main id="main" tabindex="-1" class="flex min-h-0 min-w-0 flex-1 flex-col outline-none" :class="mainClass">
      <slot />
    </main>

    <BaseDrawer v-if="!desktop" v-model:open="drawerOpen" :label="navLabel">
      <template #default="{ close }">
        <div class="flex h-[52px] flex-none items-center justify-between gap-2 pr-2 pl-4">
          <BrandMark />
          <IconButton label="Close navigation" @click="close"><IconX :size="16" stroke="1.75" /></IconButton>
        </div>
        <slot name="nav" :compact="true" :rail="false" />
      </template>
    </BaseDrawer>
  </div>
</template>
