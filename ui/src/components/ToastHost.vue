<script setup lang="ts">
// Pop-up notifications (useToasts): lifted surface cards at the bottom right (bottom centre on
// phones). The live region is always rendered so new toasts are announced politely.
import { IconAlertCircle, IconCircleCheck, IconX } from '@tabler/icons-vue'
import { dismissToast, holdToast, releaseToast, toasts } from '@/composables/useToasts'
import { IconButton } from '@/components/base'
</script>

<template>
  <div
    role="status"
    aria-live="polite"
    class="pointer-events-none fixed inset-x-0 bottom-0 z-[70] flex flex-col items-center gap-2 p-4 sm:items-end"
  >
    <div
      v-for="t in toasts"
      :key="t.id"
      class="lg-pop-card pointer-events-auto w-full max-w-[360px]"
      @mouseenter="holdToast(t.id)"
      @mouseleave="releaseToast(t.id)"
      @focusin="holdToast(t.id)"
      @focusout="releaseToast(t.id)"
    >
      <IconCircleCheck v-if="t.tone === 'success'" :size="18" stroke="1.9" aria-hidden="true" class="mt-px flex-none text-ok" />
      <IconAlertCircle v-else :size="18" stroke="1.9" aria-hidden="true" class="mt-px flex-none text-danger" />
      <div class="flex min-w-0 flex-1 flex-col gap-0.5">
        <strong class="font-semibold">{{ t.title }}</strong>
        <span v-if="t.detail" class="text-muted break-words">{{ t.detail }}</span>
      </div>
      <IconButton label="Dismiss notification" class="-my-1 -mr-1 !size-7" @click="dismissToast(t.id)">
        <IconX :size="14" stroke="1.75" />
      </IconButton>
    </div>
  </div>
</template>
