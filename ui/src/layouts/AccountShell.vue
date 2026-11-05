<script setup lang="ts">
// Account pages (own security settings). Works without the workspace data, and is the only page an
// administrator can reach while the MFA policy requires enrolment.
import { IconArrowLeft, IconShieldLock } from '@tabler/icons-vue'
import { useRoute } from 'vue-router'
import { useSessionStore } from '@/stores/session'
import { ErrorBoundary } from '@/components/base'
import StaleBuildBanner from '@/components/StaleBuildBanner.vue'
import ShellFrame from './ShellFrame.vue'

const route = useRoute()
const session = useSessionStore()
</script>

<template>
  <ShellFrame nav-label="Account navigation" main-class="overflow-auto">
    <template #nav="{ rail }">
      <nav aria-label="Account" class="flex flex-1 flex-col gap-4 overflow-auto px-2 pt-1">
        <RouterLink v-if="!session.mfaEnrolmentRequired" to="/" class="lg-nav lg-hov text-muted" v-tooltip="rail ? 'Back to tickets' : undefined">
          <IconArrowLeft :size="16" stroke="1.75" aria-hidden="true" /> <span>Back to tickets</span>
        </RouterLink>
        <div class="flex flex-col gap-px">
          <div class="lg-section-label">Account</div>
          <RouterLink :to="{ name: 'account-security' }" class="lg-nav lg-hov" v-tooltip="rail ? 'Security' : undefined" :aria-current="route.name === 'account-security' ? 'page' : undefined">
            <IconShieldLock :size="16" stroke="1.75" aria-hidden="true" />
            <span class="flex-1">Security</span>
          </RouterLink>
        </div>
      </nav>
    </template>
    <template #title>
      <span class="lg-ell font-semibold">Account</span>
    </template>

    <StaleBuildBanner />
    <ErrorBoundary>
      <RouterView />
    </ErrorBoundary>
  </ShellFrame>
</template>
