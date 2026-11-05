<script setup lang="ts">
// Agent workspace frame: sidebar (brand, global search, queues, groups, account) and the routed
// main area; below 1024 px a top bar with the workspace name, search and the user menu, and the
// navigation in a drawer (ShellFrame).
import GlobalSearch from '@/components/GlobalSearch.vue'
import QueueNav from '@/components/QueueNav.vue'
import LiveIndicator from '@/components/live/LiveIndicator.vue'
import ProblemActionBar from '@/components/ticket/ProblemActionBar.vue'
import { startLiveUpdates } from '@/composables/useLiveUpdates'
import { ErrorBoundary } from '@/components/base'
import StaleBuildBanner from '@/components/StaleBuildBanner.vue'
import { useWorkspaceStore } from '@/stores/workspace'
import ShellFrame from './ShellFrame.vue'

const workspace = useWorkspaceStore()

// One live-updates stream while the workspace is shown.
startLiveUpdates()
</script>

<template>
  <ShellFrame nav-label="Workspace navigation">
    <template #nav="{ compact, rail }">
      <div v-if="rail" class="flex justify-center pb-2">
        <GlobalSearch compact />
      </div>
      <div v-else-if="!compact" class="px-3 pb-3">
        <GlobalSearch />
      </div>
      <QueueNav :rail="rail" />
      <LiveIndicator :compact="rail" />
    </template>
    <template #title>
      <span class="lg-ell font-semibold">{{ workspace.name }}</span>
    </template>
    <template #actions>
      <GlobalSearch compact />
    </template>

    <StaleBuildBanner />
    <ProblemActionBar />
    <ErrorBoundary :home-to="{ name: 'home' }">
      <RouterView />
    </ErrorBoundary>
  </ShellFrame>
</template>
