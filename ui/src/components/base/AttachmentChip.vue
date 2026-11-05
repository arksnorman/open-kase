<script setup lang="ts">
// One file chip for both uses:
// - download (default): a button that emits `download` (message attachments);
// - staged (`removable`): an upload in the composer with progress, error state and a remove button.
import { IconAlertTriangle, IconDownload, IconFileText, IconX } from '@tabler/icons-vue'
import { computed } from 'vue'
import { fileSize } from '@/format'

const props = withDefaults(
  defineProps<{
    filename: string
    size: number
    removable?: boolean
    state?: 'uploading' | 'ready' | 'error'
    /** 0–1 while uploading. */
    progress?: number
    error?: string | null
  }>(),
  { removable: false, state: 'ready', progress: 0, error: null },
)
defineEmits<{ download: []; remove: [] }>()

const percent = computed(() => Math.round(Math.min(1, Math.max(0, props.progress)) * 100))
const statusText = computed(() => {
  if (props.state === 'uploading') return `Uploading ${percent.value}%`
  if (props.state === 'error') return props.error || 'Upload failed'
  return fileSize(props.size)
})
</script>

<template>
  <button
    v-if="!removable"
    type="button"
    class="lg-attach lg-hov"
    :aria-label="`Download ${filename}, ${fileSize(size)}`"
    @click="$emit('download')"
  >
    <IconFileText :size="16" stroke="1.75" aria-hidden="true" />
    <span class="lg-mono text-xs">{{ filename }}</span>
    <span class="text-xs text-muted">{{ fileSize(size) }}</span>
    <IconDownload :size="15" stroke="1.75" aria-hidden="true" class="text-muted" />
  </button>
  <span
    v-else
    class="lg-attach relative overflow-hidden !pr-1"
    :class="state === 'error' ? '!border-danger' : ''"
    role="group"
    :aria-label="`${filename}, ${statusText}`"
  >
    <span
      v-if="state === 'uploading'"
      aria-hidden="true"
      class="absolute inset-y-0 left-0 bg-subtle transition-[width] motion-reduce:transition-none"
      :style="{ width: `${percent}%` }"
    ></span>
    <IconAlertTriangle v-if="state === 'error'" :size="16" stroke="1.75" aria-hidden="true" class="relative text-danger" />
    <IconFileText v-else :size="16" stroke="1.75" aria-hidden="true" class="relative" />
    <span class="lg-mono lg-ell relative max-w-[220px] text-xs">{{ filename }}</span>
    <span class="relative text-xs" :class="state === 'error' ? 'text-danger' : 'text-muted'" aria-live="polite">{{ statusText }}</span>
    <button
      type="button"
      class="lg-hov relative inline-flex h-6 w-6 items-center justify-center rounded"
      :aria-label="state === 'uploading' ? `Cancel upload of ${filename}` : `Remove ${filename}`"
      @click="$emit('remove')"
    >
      <IconX :size="14" stroke="1.75" aria-hidden="true" />
    </button>
  </span>
</template>
