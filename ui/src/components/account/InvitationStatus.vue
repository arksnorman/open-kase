<script setup lang="ts">
// Pending invitation of an invited user (admin user list): expiry, or that no link is active.
import { adminApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import { RelativeTime } from '@/components/base'
import { useQuery } from '@/composables/useQuery'

const props = defineProps<{ userId: string }>()
const q = useQuery(
  () => `admin:users:invitation:${props.userId}`,
  async (signal) => {
    try {
      return await adminApi.invitation(props.userId, { signal })
    } catch (err) {
      if (err instanceof ApiError && err.is('invitationNotFound')) return null
      throw err
    }
  },
)
</script>

<template>
  <span class="text-xs text-muted">
    <template v-if="q.data.value">Link <RelativeTime :iso="q.data.value.expiresAt" future-prefix="expires" past-prefix="expired" /></template>
    <template v-else-if="q.data.value === null">No active invitation link</template>
    <template v-else-if="q.error.value">Invitation status unavailable</template>
  </span>
</template>
