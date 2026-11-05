<script setup lang="ts">
// A list or panel that failed to load: icon, the problem-details text (with its request
// reference) and "Try again". Used inside DataTable's body and by table-like lists that are
// not DataTables, so every failed list reads and recovers the same way.
import { IconAlertTriangle, IconRefresh } from '@tabler/icons-vue'
import { computed } from 'vue'
import { ApiError, errorMessage } from '@/api/errors'
import BaseButton from './BaseButton.vue'

const props = withDefaults(defineProps<{ error: unknown; title?: string; retrying?: boolean }>(), { title: 'Could not load this list' })
defineEmits<{ retry: [] }>()

const text = computed(() => (typeof props.error === 'string' ? props.error : errorMessage(props.error)))
/** Permission and validation problems will not go away by asking again. */
const retryable = computed(() => !(props.error instanceof ApiError) || props.error.status === 0 || props.error.status === 408 || props.error.status === 429 || props.error.status >= 500)
</script>

<template>
  <div role="alert" class="flex items-start gap-2.5 py-3">
    <IconAlertTriangle :size="16" stroke="1.75" aria-hidden="true" class="mt-0.5 flex-none text-danger" />
    <div class="flex min-w-0 flex-1 flex-col gap-0.5">
      <p class="m-0 font-semibold">{{ title }}</p>
      <p class="m-0 text-muted break-words">{{ text }}</p>
    </div>
    <BaseButton v-if="retryable" size="sm" :loading="retrying" @click="$emit('retry')">
      <template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>
      Try again
    </BaseButton>
  </div>
</template>
