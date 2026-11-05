<script setup lang="ts">
import { watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useSessionStore } from '@/stores/session'
import { ErrorBoundary } from '@/components/base'
import StaleBuildBanner from '@/components/StaleBuildBanner.vue'
import ToastHost from '@/components/ToastHost.vue'

const session = useSessionStore()
const router = useRouter()
const route = useRoute()

// An active session ended (expired or revoked): return to sign-in, keeping the location.
watch(
  () => session.expired,
  (expired) => {
    if (expired && !route.meta.public) void router.replace({ name: 'sign-in', query: { redirect: route.fullPath } })
  },
)

// The admin MFA policy applies to this session (at sign-in, refresh or a 403 from any route):
// only Account → Security works until enrolment is confirmed.
watch(
  () => session.mfaEnrolmentRequired,
  (required) => {
    if (required && route.name !== 'account-security') void router.replace({ name: 'account-security' })
  },
)
</script>

<template>
  <!-- Shells have their own boundary and banner; this catches the shells themselves and shell-less pages. -->
  <StaleBuildBanner v-if="route.matched.length < 2" floating />
  <ErrorBoundary fullscreen>
    <RouterView />
  </ErrorBoundary>
  <ToastHost />
</template>
