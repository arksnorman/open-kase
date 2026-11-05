<script setup lang="ts">
import { IconPlus } from '@tabler/icons-vue'
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi, ticketsApi, type TicketListQuery } from '@/api/endpoints'
import { isAbortError } from '@/api/errors'
import type { Ticket } from '@/api/types'
import { BaseAlert, BaseButton, SearchInput } from '@/components/base'
import QueueFilterBar from '@/components/QueueFilterBar.vue'
import TicketTable from '@/components/TicketTable.vue'
import { anySignal, useQuery, queryScopeSignal } from '@/composables/useQuery'
import { filtersFromQuery, filtersToParams, filtersToQuery, hasFilters, withoutFixed, type QueueFilters } from '@/filters'
import { queueBySlug } from '@/queues'
import { setViewTitle } from '@/router'
import { useWorkspaceStore } from '@/stores/workspace'

const PAGE = 50
const route = useRoute()
const router = useRouter()
const workspace = useWorkspaceStore()

const queue = computed(() => queueBySlug(String(route.params.view)))
const q = computed(() => (typeof route.query.q === 'string' ? route.query.q : ''))
const search = ref(q.value)
watch(q, (v) => (search.value = v))
watch(search, (v) => {
  if (v !== q.value) onSearchInput()
})

// Group filter (`?group=<id>` → API `groupId`, intersected with access server-side); its chip lives in the
// filter bar, which searches the groups itself.
const groupId = computed(() => (typeof route.query.group === 'string' ? route.query.group : ''))
const groupQ = useQuery(
  () => (groupId.value ? `group:${groupId.value}` : null),
  (signal) => adminApi.group(groupId.value, { signal }),
)
const groupName = computed(() => groupQ.data.value?.name)

// ---- filters (every queue; the filters a queue fixes itself are ignored) ----
const filters = computed(() => withoutFixed(filtersFromQuery(route.query), queue.value?.fixes ?? []))

function listQuery(): TicketListQuery {
  const view = queue.value!.view
  return { view, q: q.value, groupId: groupId.value || undefined, filters: filtersToParams(filters.value) }
}

const key = () => (queue.value ? `tickets:${queue.value.view}:${JSON.stringify(listQuery())}` : null)
const first = useQuery(key, (signal) => ticketsApi.list({ ...listQuery(), limit: PAGE }, { signal }))

// Additional pages are appended locally and dropped when the query key changes.
const more = shallowRef<Ticket[]>([])
const moreCursor = ref<string | null | undefined>(undefined)
const loadingMore = ref(false)
const moreError = ref<unknown>(null)
watch(key, () => {
  more.value = []
  moreCursor.value = undefined
  moreError.value = null
})

const tickets = computed(() => [...(first.data.value?.items ?? []), ...more.value])
const nextCursor = computed(() => (moreCursor.value === undefined ? (first.data.value?.nextCursor ?? null) : moreCursor.value))

async function loadMore() {
  const cursor = nextCursor.value
  if (!cursor || !queue.value) return
  const k = key()
  loadingMore.value = true
  moreError.value = null
  try {
    const page = await ticketsApi.list({ ...listQuery(), cursor, limit: PAGE }, { signal: anySignal([queryScopeSignal()]) })
    if (key() !== k) return
    more.value = [...more.value, ...page.items]
    moreCursor.value = page.nextCursor
  } catch (err) {
    if (!isAbortError(err)) moreError.value = err
  } finally {
    loadingMore.value = false
  }
}

let debounce: ReturnType<typeof setTimeout> | undefined
function onSearchInput() {
  clearTimeout(debounce)
  debounce = setTimeout(() => {
    const value = search.value.trim()
    void router.replace({ query: { ...route.query, q: value || undefined } })
  }, 300)
}

const searchInput = ref<InstanceType<typeof SearchInput> | null>(null)
function onGlobalKey(e: KeyboardEvent) {
  const target = e.target as HTMLElement | null
  const typing = target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable)
  if (e.key === '/' && !typing && !e.metaKey && !e.ctrlKey && !e.altKey) {
    e.preventDefault()
    searchInput.value?.focus()
  }
}
onMounted(() => document.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onGlobalKey)
  clearTimeout(debounce)
})

const summary = computed(() => {
  if (!first.data.value) return ''
  const n = tickets.value.length
  const more = nextCursor.value ? '+' : ''
  const where = groupName.value ? `${groupName.value} · ${workspace.name}` : workspace.name
  const breached = tickets.value.filter((t) => t.sla?.state === 'breached').length
  const dueSoon = tickets.value.filter((t) => t.sla?.state === 'dueSoon').length
  const sla = [breached ? `${breached} breached` : '', dueSoon ? `${dueSoon} due soon` : ''].filter(Boolean).join(', ')
  const filtered = hasFilters(filters.value) ? ' (filtered)' : ''
  return `${n}${more} ${n === 1 ? 'ticket' : 'tickets'} in ${where}${filtered}${q.value ? ` matching “${q.value}”` : ''}${sla ? ` · ${sla}` : ''}`
})
const title = computed(() => queue.value?.label ?? 'Unknown queue')
// The browser tab names the queue (and group) on screen instead of the route's generic "Queue".
let titledPath = ''
watch(
  () => [route.path, groupName.value ? `${title.value} · ${groupName.value}` : title.value] as const,
  ([path, name]) => {
    titledPath = path
    setViewTitle(path, name)
  },
  { immediate: true },
)
onBeforeUnmount(() => setViewTitle(titledPath, null))

function onFilters(f: QueueFilters, group: string) {
  const query = filtersToQuery(f, route.query)
  query.group = group || undefined
  if (!group) delete query.group
  void router.replace({ query })
}

// ---- filtered empty state ----
/** Search, the group filter or filters narrow this list. */
const narrowed = computed(() => !!q.value || !!groupId.value || hasFilters(filters.value))
/** Back to the unfiltered queue. */
function clearFilters() {
  clearTimeout(debounce)
  search.value = ''
  void router.replace({ query: {} })
}
</script>

<template>
  <div class="flex h-[52px] flex-none items-center gap-2 border-b border-border bg-surface px-5 sm:gap-4">
    <!-- Below 640 px the shell's top bar names the project and the page heading the queue. -->
    <nav aria-label="Breadcrumb" class="hidden min-w-0 items-center gap-1.5 text-muted sm:flex">
      <span class="lg-ell">{{ workspace.name }}</span>
      <span aria-hidden="true">/</span>
      <span aria-current="page" class="lg-ell font-semibold text-text">{{ queue?.label ?? 'Queue' }}</span>
    </nav>
    <div class="hidden flex-1 sm:block"></div>
    <form role="search" class="min-w-0 flex-1 sm:w-80 sm:flex-none sm:shrink" @submit.prevent>
      <SearchInput
        id="ticket-search"
        ref="searchInput"
        v-model="search"
        label="Search tickets"
        placeholder="Search key, subject, requester"
        shortcut="/"
      />
    </form>
    <BaseButton variant="primary" class="!h-[34px]" :to="{ name: 'ticket-new' }">
      <template #icon><IconPlus :size="16" stroke="1.75" aria-hidden="true" /></template>
      New ticket
    </BaseButton>
  </div>

  <div class="flex flex-none flex-col gap-3 px-5 pt-[18px] pb-3">
    <div class="flex items-end justify-between gap-4">
      <div class="flex min-w-0 flex-col gap-0.5">
        <div class="flex items-center gap-2">
          <h1 class="m-0 min-w-0 text-lg font-semibold tracking-[-0.01em]">{{ title }}</h1>
        </div>
        <p class="m-0 text-muted" aria-live="polite">{{ first.loading.value && !first.data.value ? 'Loading tickets…' : summary }}</p>
      </div>
    </div>

    <div v-if="queue" class="flex flex-wrap items-center gap-2">
      <QueueFilterBar :filters="filters" :group-ids="groupId ? [groupId] : []" :fixed="queue.fixes" @change="onFilters" />
    </div>
  </div>

  <div class="flex min-h-0 flex-1 flex-col overflow-auto px-5 pb-5">
    <BaseAlert v-if="!queue" error="This queue does not exist." />
    <TicketTable
      v-if="queue && (first.data.value || first.error.value)"
      :tickets="tickets"
      :from-slug="queue.slug"
      :loading="first.loading.value"
      :error="first.error.value"
      :filtered="narrowed"
      :has-more="!!nextCursor"
      :loading-more="loadingMore"
      :label="`${queue.label} in ${workspace.name}`"
      :empty-text="queue.empty"
      :assignee-redundant="queue.view === 'myWork' || queue.view === 'unassigned'"
      @retry="first.refetch"
      @clear-filters="clearFilters"
      @load-more="loadMore"
    />
    <BaseAlert v-if="moreError" :error="moreError" class="mt-2" />
  </div>
</template>
