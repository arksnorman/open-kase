<script setup lang="ts">
// Shown by the shells (and App for shell-less pages) once a lazy import failed after an update.
// The live region is always rendered so the message is announced politely when it appears.
import { IconRefresh } from '@tabler/icons-vue'
import { BaseAlert, BaseButton } from '@/components/base'
import { reloadApp, staleBuild } from '@/staleBuild'

defineProps<{ floating?: boolean }>()
</script>

<template>
  <div aria-live="polite" :class="floating ? 'fixed inset-x-0 top-0 z-50 flex justify-center p-3' : staleBuild ? 'flex-none px-5 pt-3' : ''">
    <BaseAlert v-if="staleBuild" tone="info" :class="floating ? 'w-full max-w-xl shadow-sm' : ''">
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <span class="flex-1">This tab is out of date after an update. Reload to continue.</span>
        <BaseButton size="sm" variant="primary" @click="reloadApp">
          <template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>
          Reload
        </BaseButton>
      </div>
    </BaseAlert>
  </div>
</template>
