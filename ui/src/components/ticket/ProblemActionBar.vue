<script setup lang="ts">
// Shows the `createFollowUp` action of a failed request (problem `actions`, sent with
// `ticketArchived`) as a button, wherever the failing request was made. The original request
// is never retried. On the archived ticket's own page the ticket header offers the same action,
// so the bar stays hidden there.
import { computed, onScopeDispose, ref, shallowRef, watch } from 'vue'
import { useRoute } from 'vue-router'
import { setProblemActionHook } from '@/api/client'
import type { ProblemAction } from '@/api/errors'
import { BaseAlert, BaseButton } from '@/components/base'
import FollowUpDialog from './FollowUpDialog.vue'

const route = useRoute()
const action = shallowRef<{ action: ProblemAction; ticketNumber: string } | null>(null)
const dialogOpen = ref(false)

const HREF = /^\/api\/v1\/tickets\/([^/]+)\/follow-up$/
setProblemActionHook((err) => {
  const a = err.action('createFollowUp')
  const m = a ? HREF.exec(a.href) : null
  if (!a || !m) return
  action.value = { action: a, ticketNumber: decodeURIComponent(m[1]!).replace(/^#/, '') }
})
onScopeDispose(() => setProblemActionHook(null))
// The hint belongs to the screen where the request failed.
watch(
  () => route.fullPath,
  () => {
    if (!dialogOpen.value) action.value = null
  },
)

const visible = computed(() => {
  const a = action.value
  if (!a) return false
  return !(route.name === 'ticket' && String(route.params.ticketNumber).replace(/^#/, '') === a.ticketNumber)
})
</script>

<template>
  <div v-if="visible && action" class="flex-none border-b border-border bg-surface px-5 py-2">
    <BaseAlert tone="warn">
      <span class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <span class="flex-1">#{{ action.ticketNumber }} is archived and cannot be changed. Further work needs a linked follow-up ticket.</span>
        <BaseButton size="sm" variant="primary" @click="dialogOpen = true">{{ action.action.title }}</BaseButton>
        <BaseButton size="sm" variant="ghost" @click="action = null">Dismiss</BaseButton>
      </span>
    </BaseAlert>
    <FollowUpDialog
      v-model:open="dialogOpen"
      :ticket-number="action.ticketNumber"
      :href="action.action.href"
    />
  </div>
</template>
