<script setup lang="ts">
// Inline alert. `error` accepts a string or any thrown value (ApiError → problem-details text).
// danger/warn: tinted callouts. success/info: notices on a lifted surface card (green check or
// info icon); `title` adds a bold first line with the text as the muted detail.
import { IconAlertTriangle, IconCircleCheck, IconInfoCircle } from '@tabler/icons-vue'
import { computed } from 'vue'
import { errorMessage } from '@/api/errors'

const props = withDefaults(defineProps<{ error?: unknown; tone?: 'danger' | 'warn' | 'info' | 'success'; title?: string }>(), { tone: 'danger' })
const text = computed(() => (props.error === undefined || props.error === null ? '' : typeof props.error === 'string' ? props.error : errorMessage(props.error)))
const card = computed(() => props.tone === 'info' || props.tone === 'success')
</script>

<template>
  <div
    v-if="text || title || $slots.default"
    :role="card ? 'status' : 'alert'"
    :class="tone === 'warn' ? 'lg-alert-warn' : card ? 'lg-pop-card' : 'lg-alert-danger'"
  >
    <IconCircleCheck v-if="tone === 'success'" :size="16" stroke="1.9" aria-hidden="true" class="mt-px flex-none text-ok" />
    <IconInfoCircle v-else-if="tone === 'info'" :size="16" stroke="1.75" aria-hidden="true" class="mt-px flex-none text-muted" />
    <IconAlertTriangle v-else :size="16" stroke="1.75" aria-hidden="true" class="mt-px flex-none" />
    <div class="flex min-w-0 flex-1 flex-col gap-0.5">
      <strong v-if="title" class="font-semibold">{{ title }}</strong>
      <div :class="title ? 'text-muted' : ''"><slot>{{ text }}</slot></div>
    </div>
  </div>
</template>
