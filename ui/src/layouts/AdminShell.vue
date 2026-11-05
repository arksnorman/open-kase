<script setup lang="ts">
import {
  IconArrowLeft,
  IconBuildingSkyscraper,
  IconChartBar,
  IconClockHour4,
  IconListDetails,
  IconMessageBolt,
  IconPlayerPause,
  IconPlug,
  IconSettings,
  IconTags,
  IconUser,
  IconUsers,
  IconWebhook,
} from '@tabler/icons-vue'
import { useRoute } from 'vue-router'
import { ErrorBoundary } from '@/components/base'
import StaleBuildBanner from '@/components/StaleBuildBanner.vue'
import ShellFrame from './ShellFrame.vue'

const route = useRoute()

const items = [
  { name: 'admin-users', label: 'Users', icon: IconUser },
  { name: 'admin-groups', label: 'Groups', icon: IconUsers },
  { name: 'admin-sla', label: 'SLA', icon: IconClockHour4 },
  { name: 'admin-macros', label: 'Canned replies', icon: IconMessageBolt },
  { name: 'admin-tags', label: 'Tags', icon: IconTags },
  { name: 'admin-hold-reasons', label: 'Hold reasons', icon: IconPlayerPause },
  { name: 'admin-organisations', label: 'Organisations', icon: IconBuildingSkyscraper },
  { name: 'admin-integrations', label: 'Integrations', icon: IconPlug },
  { name: 'admin-webhooks', label: 'Webhooks', icon: IconWebhook },
  { name: 'admin-logs', label: 'Logs', icon: IconListDetails },
  { name: 'admin-reports', label: 'Reports', icon: IconChartBar },
  { name: 'admin-settings', label: 'Configuration', icon: IconSettings },
] as const
</script>

<template>
  <ShellFrame nav-label="Settings navigation" main-class="overflow-auto">
    <template #nav="{ rail }">
      <nav aria-label="Settings" class="flex flex-1 flex-col gap-px overflow-auto px-2 pt-1">
        <RouterLink to="/" class="lg-nav lg-hov mb-3 text-muted" v-tooltip="rail ? 'Back to tickets' : undefined">
          <IconArrowLeft :size="16" stroke="1.75" aria-hidden="true" /> <span>Back to tickets</span>
        </RouterLink>
        <RouterLink
          v-for="s in items"
          :key="s.name"
          :to="{ name: s.name }"
          class="lg-nav lg-hov"
          v-tooltip="rail ? s.label : undefined"
          :aria-current="route.name === s.name || (s.name === 'admin-users' && route.name === 'admin-user') ? 'page' : undefined"
        >
          <component :is="s.icon" :size="16" stroke="1.75" aria-hidden="true" />
          <span class="flex-1">{{ s.label }}</span>
        </RouterLink>
      </nav>
    </template>
    <template #title>
      <span class="lg-ell font-semibold">Settings</span>
    </template>

    <StaleBuildBanner />
    <ErrorBoundary>
      <RouterView />
    </ErrorBoundary>
  </ShellFrame>
</template>
