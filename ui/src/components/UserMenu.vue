<script setup lang="ts">
import { IconAdjustmentsHorizontal, IconLogout, IconSelector, IconSettings, IconShieldLock, IconUser } from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import { useRouter } from 'vue-router'
import { roleLabel } from '@/format'
import { usePreferencesStore } from '@/stores/preferences'
import { useSessionStore } from '@/stores/session'
import PreferencesDialog from './PreferencesDialog.vue'
import ThemeSwitcher from './ThemeSwitcher.vue'

/** `compact`: the avatar alone (icon rail, narrow top bar); the menu then shows the name and role itself. */
const props = withDefaults(defineProps<{ compact?: boolean }>(), { compact: false })

const session = useSessionStore()
const prefs = usePreferencesStore()
const router = useRouter()
const open = ref(false)
const prefsOpen = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLButtonElement | null>(null)
const panelId = useId()
/** The Preferences button sits in the closed menu; focus returns to the trigger instead. */
const menuButton = () => trigger.value

const role = computed(() => (session.user ? roleLabel(session.isAdmin ? 'admin' : 'agent') : ''))

// Full sidebar: opens above the trigger, inset like the trigger. Rail: above the avatar, extending right.
// Narrow top bar: drops below the avatar, right-aligned.
const menuPos = computed(() => {
  if (!props.compact) return 'bottom-full left-3 right-3 mb-1 origin-bottom [--um-y:4px]'
  return 'bottom-full left-2 mb-1 w-[260px] origin-bottom-left [--um-y:4px] max-lg:top-full max-lg:bottom-auto max-lg:left-auto max-lg:right-0 max-lg:mt-1 max-lg:mb-0 max-lg:origin-top-right max-lg:[--um-y:-4px]'
})

function close(returnFocus = true) {
  open.value = false
  if (returnFocus) trigger.value?.focus({ preventScroll: true })
}
function onOutside(e: PointerEvent) {
  const t = e.target as Node | null
  if (t && t.isConnected && root.value && !root.value.contains(t)) close(false)
}
function onEsc(e: KeyboardEvent) {
  if (!open.value) return
  e.preventDefault()
  e.stopPropagation()
  close()
}
function onFocusOut(e: FocusEvent) {
  const next = e.relatedTarget as Node | null
  if (open.value && next && root.value && !root.value.contains(next)) close(false)
}
watch(open, async (v) => {
  if (v) {
    document.addEventListener('pointerdown', onOutside, true)
    await nextTick()
    root.value?.querySelector<HTMLElement>(`#${CSS.escape(panelId)} [data-first]`)?.focus({ preventScroll: true })
  } else document.removeEventListener('pointerdown', onOutside, true)
})
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOutside, true))

async function signOut() {
  open.value = false
  await session.logout()
  await router.push({ name: 'sign-in' })
}

const item = 'flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left text-[13px] text-text no-underline hover:bg-[var(--kase-hover)] focus-visible:bg-[var(--kase-hover)]'
</script>

<template>
  <div ref="root" class="relative" :class="compact ? 'flex flex-none' : 'px-3 pt-2 pb-3'" @keydown.esc="onEsc" @focusout="onFocusOut">

    <button
      ref="trigger"
      type="button"
      class="flex items-center text-left"
      :class="
        compact
          ? 'rounded-full border border-transparent p-0.5 hover:border-border hover:bg-[var(--kase-hover)]'
          : 'w-full gap-2.5 rounded-xl border border-transparent bg-subtle px-2.5 py-2 transition-colors hover:border-border hover:bg-[var(--kase-hover)]'
      "
      :aria-label="`Account and theme settings${session.user ? `, ${session.user.name}` : ''}`"
      v-tooltip="compact ? 'Account and theme settings' : undefined"
      aria-haspopup="dialog"
      :aria-expanded="open"
      :aria-controls="panelId"
      @click="open = !open"
    >
      <span aria-hidden="true" class="flex h-8 w-8 flex-none items-center justify-center rounded-full bg-sel text-text"><IconUser :size="17" stroke="1.75" /></span>
      <template v-if="!compact">
        <span class="min-w-0 flex-1 leading-tight">
          <span class="block truncate text-[13px] font-semibold text-text">{{ session.user?.name }}</span>
          <span class="block truncate text-[11px] text-muted">{{ role }}</span>
        </span>
        <IconSelector :size="16" stroke="1.75" aria-hidden="true" class="flex-none text-muted" />
      </template>
    </button>

    <Transition name="um-pop">
      <div
        v-if="open"
        :id="panelId"
        role="group"
        aria-label="Account"
        class="absolute z-50 overflow-hidden rounded-xl border border-border bg-surface shadow-[var(--kase-shadow-pop)]"
        :class="menuPos"
      >
        <div class="border-b border-line px-3.5 py-3">
          <p class="truncate text-[13px] font-medium text-text">{{ session.user?.name }}</p>
          <p class="truncate text-[11px] text-muted">{{ session.user?.email }}</p>
          <p v-if="compact && role" class="truncate text-[11px] text-muted">{{ role }}</p>
          <p v-if="session.user?.username" class="lg-mono truncate text-[11px] text-muted">{{ session.user.username }}</p>
        </div>
        <div class="flex flex-col gap-1.5 border-b border-line px-3.5 py-3">
          <span class="text-[11px] font-medium text-muted">Theme</span>
          <ThemeSwitcher compact />
          <span v-if="prefs.syncError" class="text-xs text-warn">{{ prefs.syncError }}</span>
        </div>
        <div class="flex flex-col p-1.5">
          <button type="button" data-first :class="item" @click="open = false; prefsOpen = true">
            <IconAdjustmentsHorizontal :size="16" stroke="1.75" aria-hidden="true" class="flex-none" />
            Preferences
          </button>
          <RouterLink :to="{ name: 'account-security' }" :class="item" @click="open = false">
            <IconShieldLock :size="16" stroke="1.75" aria-hidden="true" class="flex-none" />
            Security
          </RouterLink>
          <RouterLink v-if="session.isAdmin && !session.mfaEnrolmentRequired" :to="{ name: 'admin' }" :class="item" @click="open = false">
            <IconSettings :size="16" stroke="1.75" aria-hidden="true" class="flex-none" />
            Settings
          </RouterLink>
          <button type="button" :class="item" class="!text-danger hover:!bg-danger-bg focus-visible:!bg-danger-bg" @click="signOut">
            <IconLogout :size="16" stroke="1.75" aria-hidden="true" class="flex-none" />
            Sign out
          </button>
        </div>
      </div>
    </Transition>
    <PreferencesDialog v-model:open="prefsOpen" :return-focus="menuButton" />
  </div>
</template>

<style scoped>
.um-pop-enter-active,
.um-pop-leave-active {
  transition:
    opacity 0.15s ease,
    transform 0.15s ease;
}
.um-pop-enter-from,
.um-pop-leave-to {
  opacity: 0;
  transform: translateY(var(--um-y, 4px));
}
</style>
