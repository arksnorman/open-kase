<script setup lang="ts">
import { IconUsers } from '@tabler/icons-vue'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { ticketsApi } from '@/api/endpoints'
import { useQuery } from '@/composables/useQuery'
import { QUEUES } from '@/queues'
import { useWorkspaceStore } from '@/stores/workspace'

/** `rail`: the collapsed sidebar shows icons only; each link gets its name as a tooltip. */
defineProps<{ rail?: boolean }>()
const workspace = useWorkspaceStore()
const route = useRoute()

const activeGroup = computed(() => (route.name === 'queue' && typeof route.query.group === 'string' ? route.query.group : null))

// Counts are a convenience; navigation works if they fail to load.
const counts = useQuery(
  () => 'queue-counts',
  (signal) => ticketsApi.queueCounts({ signal }),
)

const activeSlug = computed(() => (route.name === 'queue' ? String(route.params.view) : null))
</script>

<template>
  <nav aria-label="Queues" class="flex flex-1 flex-col gap-4 overflow-auto px-2">
    <div class="flex flex-col gap-px">
      <div class="lg-section-label">Queues</div>
      <RouterLink
        v-for="q in QUEUES"
        :key="q.slug"
        :to="{ name: 'queue', params: { view: q.slug } }"
        class="lg-nav lg-hov"
        v-tooltip="rail ? q.label : undefined"
        :aria-current="activeSlug === q.slug && !(q.slug === 'all' && activeGroup) ? 'page' : undefined"
      >
        <component :is="q.icon" :size="16" stroke="1.75" aria-hidden="true" />
        <span class="flex-1">{{ q.label }}</span>
        <template v-if="q.count && counts.data.value">
          <span
            v-if="q.warnCount && counts.data.value[q.count] > 0"
            class="lg-count-warn"
            :aria-label="`${counts.data.value[q.count]} tickets`"
          >{{ counts.data.value[q.count] }}</span>
          <span v-else class="lg-count" :aria-label="`${counts.data.value[q.count]} tickets`">{{ counts.data.value[q.count] }}</span>
        </template>
      </RouterLink>
    </div>

    <div v-if="workspace.myGroups.length" class="flex flex-col gap-px">
      <div class="lg-section-label" id="nav-groups">Your groups</div>
      <ul aria-labelledby="nav-groups" class="flex flex-col gap-px">
        <li v-for="g in workspace.myGroups" :key="g.id">
          <RouterLink
            :to="{ name: 'queue', params: { view: 'all' }, query: { group: g.id } }"
            class="lg-nav lg-hov"
            :aria-current="activeSlug === 'all' && activeGroup === g.id ? 'page' : undefined"
            :aria-label="`All tickets in ${g.name}`"
            v-tooltip="rail ? g.name : undefined"
          >
            <IconUsers :size="16" stroke="1.75" aria-hidden="true" />
            <span class="lg-ell flex-1">{{ g.name }}</span>
          </RouterLink>
        </li>
      </ul>
    </div>

  </nav>
</template>
