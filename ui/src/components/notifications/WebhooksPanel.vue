<script setup lang="ts">
import EntityIcon from '@/components/base/EntityIcon.vue'
// Webhook endpoints (admins): a list with a health strip per webhook, one page per webhook (figures,
// Settings, Deliveries), pause/resume, test delivery, delete and a three-step New webhook dialog.
// Credentials are write-only. The selected webhook lives in the URL (?webhook=<id>&tab=deliveries).
import { IconArrowLeft, IconExternalLink, IconLock, IconPlayerPause, IconPlayerPlay, IconSend, IconTrash } from '@tabler/icons-vue'
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { webhooksApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { Webhook, WebhookInput } from '@/api/types'
import {
  BaseAlert,
  BaseBadge,
  BaseButton,
  BaseCard,
  BaseModal,
  BaseTabs,
  EmptyState,
  ErrorState,
  LoadingState,
  RelativeTime,
  type TabDef,
} from '@/components/base'
import { useDeliveryPoll } from '@/composables/useDeliveryPoll'
import { usePagedQuery } from '@/composables/usePagedQuery'
import { invalidate, useQuery } from '@/composables/useQuery'
import { isTerminal } from '@/deliveries'
import { useSessionStore } from '@/stores/session'
import { authSummary, eventShort, health, percent } from '@/webhookLabels'
import DeliveryLog from './DeliveryLog.vue'
import DeliveryResult from './DeliveryResult.vue'
import WebhookForm from './WebhookForm.vue'

const session = useSessionStore()
const route = useRoute()
const router = useRouter()

const DOCS_URL = 'https://github.com/arksnorman/open-kase/blob/main/docs/webhooks.md#authenticating-deliveries'

const listKey = 'webhooks'
const list = usePagedQuery<Webhook>(
  () => listKey,
  (cursor, signal) => webhooksApi.list(cursor, { signal }),
)
const types = useQuery(() => 'webhook-event-types', (signal) => webhooksApi.eventTypes({ signal }))
const eventTypes = computed(() => types.data.value?.items ?? [])

// The selected webhook is read fresh when its page opens (statistics change quickly).
const selectedId = computed(() => (typeof route.query.webhook === 'string' ? route.query.webhook : null))
const inList = computed(() => list.items.value.find((w) => w.id === selectedId.value) ?? null)
const one = useQuery(
  () => (selectedId.value ? `webhook:${selectedId.value}` : null),
  (signal) => webhooksApi.get(selectedId.value!, { signal }),
)
const selected = computed(() => (one.data.value?.id === selectedId.value ? one.data.value : inList.value))

type Tab = 'settings' | 'deliveries'
const tabs: TabDef<Tab>[] = [
  { id: 'settings', label: 'Settings' },
  { id: 'deliveries', label: 'Deliveries' },
]
const tab = computed<Tab>({
  get: () => (route.query.tab === 'deliveries' ? 'deliveries' : 'settings'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v === 'settings' ? undefined : v } }),
})

function open(w: Webhook | null) {
  if (w) invalidate(`webhook:${w.id}`)
  void router.push({ query: { ...route.query, webhook: w?.id, tab: undefined } })
  test.reset()
  testError.value = null
  actionError.value = null
  saveError.value = null
}

function replace(w: Webhook) {
  list.replace(w, (x) => x.id === w.id)
  if (one.data.value?.id === w.id) one.set(w)
}

// ---- New webhook (three steps) ----
const creating = ref(false)
const step = ref<1 | 2 | 3>(1)
const createBusy = ref(false)
const createError = ref<unknown>(null)
const createForm = ref<InstanceType<typeof WebhookForm> | null>(null)
const createdHook = ref<Webhook | null>(null)
const createdTest = useDeliveryPoll()
const createdTestBusy = ref(false)
const createdTestError = ref<unknown>(null)
const STEP_TITLES = ['Endpoint', 'Events', 'Security'] as const
const stepSection = computed(() => (['endpoint', 'events', 'security'] as const)[step.value - 1]!)
/** The page heading's "New webhook" button (WebhooksAdmin) opens the dialog. */
const canCreate = computed(() => !selectedId.value && eventTypes.value.length > 0)
function openCreate() {
  createError.value = null
  createdHook.value = null
  createdTest.reset()
  createdTestError.value = null
  step.value = 1
  createForm.value?.reset()
  creating.value = true
}
defineExpose({ openCreate, canCreate, listView: computed(() => !selectedId.value) })

const nextDisabled = computed(() => (step.value === 1 ? !createForm.value?.endpointOk : step.value === 2 ? !createForm.value?.eventsOk : false))
async function create() {
  if (!createForm.value) return
  createBusy.value = true
  createError.value = null
  try {
    createdHook.value = await webhooksApi.create(createForm.value.input)
    invalidate(listKey)
  } catch (err) {
    createError.value = err
  } finally {
    createBusy.value = false
  }
}
// A validation error on an earlier step takes the person back to it.
watch(createError, (err) => {
  if (!(err instanceof ApiError) || !err.fieldErrors.length) return
  if (err.field('name') || err.field('url')) step.value = 1
  else if (err.field('eventTypes') || err.field('includeAgentNotes')) step.value = 2
})
async function sendCreatedTest() {
  const w = createdHook.value
  if (!w) return
  createdTestBusy.value = true
  createdTestError.value = null
  createdTest.reset()
  try {
    const res = await webhooksApi.test(w.id)
    createdTest.start(res.delivery, (id, signal) => webhooksApi.delivery(w.id, id, { signal }))
  } catch (err) {
    createdTestError.value = err
  } finally {
    createdTestBusy.value = false
  }
}
function openCreated() {
  const w = createdHook.value
  creating.value = false
  if (w) open(w)
}

// ---- Settings tab (one Save for the whole tab) ----
const settings = ref<InstanceType<typeof WebhookForm> | null>(null)
const saveBusy = ref(false)
const saveError = ref<unknown>(null)
async function saveSettings() {
  const w = selected.value
  if (!w || !settings.value) return
  const input: WebhookInput = settings.value.input
  if (!Object.keys(input).length) return
  saveBusy.value = true
  saveError.value = null
  try {
    replace(await webhooksApi.update(w, input))
    settings.value?.reset() // clears typed secrets
  } catch (err) {
    if (err instanceof ApiError && err.is('versionConflict')) {
      void one.refetch()
      invalidate(listKey)
    }
    saveError.value = err
  } finally {
    saveBusy.value = false
  }
}

// ---- Pause / resume ----
const pauseBusy = ref(false)
const actionError = ref<unknown>(null)
const pausingId = ref<string | null>(null)
async function setActive(w: Webhook, active: boolean) {
  if (pauseBusy.value) return
  pauseBusy.value = true
  pausingId.value = w.id
  actionError.value = null
  try {
    replace(await webhooksApi.update(w, { active }))
    invalidate(listKey)
  } catch (err) {
    actionError.value = err
    if (selectedId.value) void one.refetch()
    invalidate(listKey)
    void list.refetch()
  } finally {
    pauseBusy.value = false
    pausingId.value = null
  }
}

// ---- Test ----
const test = useDeliveryPoll()
const testBusy = ref(false)
const testError = ref<unknown>(null)
async function sendTest() {
  const w = selected.value
  if (!w) return
  testBusy.value = true
  testError.value = null
  test.reset()
  try {
    const res = await webhooksApi.test(w.id)
    test.start(res.delivery, (id, signal) => webhooksApi.delivery(w.id, id, { signal }))
  } catch (err) {
    testError.value = err
  } finally {
    testBusy.value = false
  }
}
// The figures change once the test delivery has settled.
watch(
  () => test.delivery.value?.status,
  (status) => {
    if (status && isTerminal(status)) void one.refetch()
  },
)

// ---- Delete ----
const deleteOpen = ref(false)
const deleteBusy = ref(false)
const deleteError = ref<unknown>(null)
async function remove() {
  const w = selected.value
  if (!w) return
  deleteBusy.value = true
  deleteError.value = null
  try {
    await webhooksApi.remove(w.id)
    deleteOpen.value = false
    open(null)
    invalidate(listKey)
  } catch (err) {
    deleteError.value = err
  } finally {
    deleteBusy.value = false
  }
}

const figures = computed(() => {
  const s = selected.value?.stats
  return [
    { label: 'Sent in 24h', value: String(s?.sentLast24h ?? 0) },
    { label: 'Delivered first try', value: percent(s?.deliveredFirstTryRate ?? null) },
    { label: 'Retrying now', value: String(s?.retrying ?? 0) },
    { label: 'Typical response', value: s?.medianResponseMs != null ? `${s.medianResponseMs} ms` : '—' },
  ]
})
const BAR_TONE = { delivered: 'bg-ok', retrying: 'bg-warn', failed: 'bg-danger' } as const
</script>

<template>
  <div class="flex flex-col gap-4">
    <BaseAlert v-if="types.error.value" :error="types.error.value" />

    <!-- Webhook page -->
    <template v-if="selectedId">
      <div>
        <BaseButton size="sm" variant="ghost" @click="open(null)">
          <template #icon><IconArrowLeft :size="14" stroke="1.75" aria-hidden="true" /></template>
          All webhooks
        </BaseButton>
      </div>
      <BaseAlert v-if="one.error.value && !selected" :error="one.error.value instanceof ApiError && one.error.value.status === 404 ? 'This webhook no longer exists.' : one.error.value" />
      <LoadingState v-else-if="!selected" />
      <template v-if="selected">
        <div class="flex flex-wrap items-start gap-3">
          <div class="flex min-w-0 flex-1 flex-wrap items-center gap-2">
            <h2 class="m-0 min-w-0 text-[15px] font-semibold break-words">{{ selected.name }}</h2>
            <BaseBadge :tone="selected.active ? 'success' : 'neutral'">{{ selected.active ? 'Active' : 'Paused' }}</BaseBadge>
            <BaseBadge v-if="selected.includeAgentNotes" tone="warn">Agent notes</BaseBadge>
            <p class="lg-mono m-0 basis-full text-xs break-all text-muted">{{ selected.url }}</p>
          </div>
          <div class="flex flex-wrap gap-2">
            <BaseButton size="sm" :loading="testBusy" @click="sendTest">
              <template #icon><IconSend :size="14" stroke="1.75" aria-hidden="true" /></template>
              Send test
            </BaseButton>
            <BaseButton size="sm" :loading="pauseBusy" @click="setActive(selected, !selected.active)">
              <template #icon><component :is="selected.active ? IconPlayerPause : IconPlayerPlay" :size="14" stroke="1.75" aria-hidden="true" /></template>
              {{ selected.active ? 'Pause' : 'Resume' }}
            </BaseButton>
            <BaseButton size="sm" variant="danger" @click="deleteError = null; deleteOpen = true">
              <template #icon><IconTrash :size="14" stroke="1.75" aria-hidden="true" /></template>
              Delete
            </BaseButton>
          </div>
        </div>
        <BaseAlert v-if="actionError" :error="actionError" />
        <BaseAlert v-if="!selected.active" tone="info">
          This webhook is paused: it receives nothing, and a test delivery is skipped. Resume it to send events again.
        </BaseAlert>
        <BaseAlert v-if="testError" :error="testError" />
        <DeliveryResult v-if="test.delivery.value" :delivery="test.delivery.value" :polling="test.polling.value" :gave-up="test.gaveUp.value" />
        <BaseAlert v-if="test.error.value" :error="test.error.value" />

        <dl class="m-0 grid grid-cols-2 gap-3 lg:grid-cols-4">
          <div v-for="f in figures" :key="f.label" class="lg-panel flex flex-col gap-0.5 p-3">
            <dt class="text-xs text-muted">{{ f.label }}</dt>
            <dd class="m-0 text-xl font-semibold tabular-nums">{{ f.value }}</dd>
          </div>
        </dl>

        <BaseTabs v-model="tab" :tabs="tabs" label="Webhook sections" variant="compact">
          <template #settings>
            <form class="mt-4 flex max-w-[760px] flex-col gap-4" novalidate @submit.prevent="saveSettings">
              <WebhookForm ref="settings" :event-types="eventTypes" :can-grant-notes="session.isAdmin" :initial="selected" :error="saveError" />
              <div class="flex justify-end gap-2">
                <BaseButton :disabled="!settings?.dirty" @click="settings?.reset(); saveError = null">Reset</BaseButton>
                <BaseButton type="submit" variant="primary" :loading="saveBusy" :disabled="!settings?.dirty || !settings?.endpointOk || !settings?.eventsOk">Save</BaseButton>
              </div>
            </form>
          </template>
          <template #deliveries>
            <div class="mt-4">
              <DeliveryLog
                variant="endpoint"
                :label="`Deliveries to ${selected.name}`"
                :reload-key="`${selected.id}:${test.delivery.value?.status ?? ''}`"
                :fetch-page="(q, signal) => webhooksApi.deliveries(selected!.id, q, { signal })"
                :retry="(d) => webhooksApi.retryDelivery(selected!.id, d.id)"
              />
            </div>
          </template>
        </BaseTabs>
      </template>
    </template>

    <!-- List -->
    <template v-else>
      <ErrorState v-if="list.error.value && !list.items.value.length" :error="list.error.value" :retrying="list.loading.value" @retry="list.refetch" />
      <LoadingState v-else-if="list.loading.value && !list.items.value.length" />
      <div v-else-if="!list.items.value.length" class="lg-panel">
        <EmptyState title="No webhooks yet" description="Add an endpoint to tell another system when tickets change.">
          <template #icon><EntityIcon kind="webhook" size="lg" /></template>
        </EmptyState>
      </div>
      <ul v-else class="lg-panel m-0 flex list-none flex-col divide-y divide-border p-0" aria-label="Webhooks">
        <li
          v-for="w in list.items.value"
          :key="w.id"
          class="grid grid-cols-[auto_minmax(0,1fr)] items-start gap-x-3 gap-y-3 p-3 min-[820px]:grid-cols-[auto_minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,0.9fr)_auto] min-[820px]:items-center min-[820px]:gap-x-4 sm:p-4"
        >
          <EntityIcon kind="webhook" size="md" class="!rounded-full" :class="w.active ? '' : 'opacity-75'" />
          <div class="flex min-w-0 flex-col gap-1" :class="w.active ? '' : 'opacity-75'">
            <div class="lg-ell text-[13px] font-semibold" v-tooltip.truncated="w.name">{{ w.name }}</div>
            <div class="lg-mono lg-ell text-[11px] text-muted" v-tooltip.truncated="w.url">{{ w.url }}</div>
            <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
              <BaseBadge :tone="w.active ? 'success' : 'neutral'">{{ w.active ? 'Active' : 'Paused' }}</BaseBadge>
              <BaseBadge v-if="w.includeAgentNotes" tone="warn" v-tooltip="'Receives agent-only notes'">
                <IconLock :size="11" stroke="1.9" aria-hidden="true" class="mr-0.5 inline" />Agent notes
              </BaseBadge>
              <span class="text-xs text-muted">{{ authSummary(w) }}</span>
            </div>
          </div>
          <div class="col-span-2 flex min-w-0 flex-col gap-1 min-[820px]:col-span-1" :class="w.active ? '' : 'opacity-75'">
            <span class="inline-flex h-4 flex-none items-end gap-[3px]" role="img" :aria-label="health(w).summary" v-tooltip="health(w).summary">
              <span v-for="n in 10 - health(w).bars.length" :key="`e${n}`" class="h-3 w-1.5 rounded-sm bg-border" />
              <span v-for="(b, i) in health(w).bars" :key="i" class="h-4 w-1.5 rounded-sm" :class="BAR_TONE[b]" />
            </span>
            <span v-if="!w.active && w.stats.pausedAt" class="text-xs text-muted"><RelativeTime :iso="w.stats.pausedAt" past-prefix="Paused" /></span>
            <span v-else-if="w.active && health(w).failing" class="text-xs font-medium text-danger">
              Failing: {{ health(w).failed }} of the last {{ health(w).bars.length }} failed
            </span>
            <span v-else-if="w.stats.lastSentAt" class="text-xs text-muted">
              Last sent <RelativeTime :iso="w.stats.lastSentAt" /> · {{ percent(w.stats.deliveredFirstTryRate) }} first try in 24h
            </span>
            <span v-else class="text-xs text-muted">No deliveries in the last 24 hours</span>
          </div>
          <ul class="col-span-2 m-0 flex list-none flex-wrap gap-1 p-0 min-[820px]:col-span-1" aria-label="Events">
            <li v-for="t in (w.eventTypes ?? []).slice(0, 3)" :key="t"><BaseBadge>{{ eventShort(t) }}</BaseBadge></li>
            <li v-if="(w.eventTypes ?? []).length > 3"><BaseBadge v-tooltip="(w.eventTypes ?? []).slice(3).map(eventShort).join(', ')">+{{ (w.eventTypes ?? []).length - 3 }}</BaseBadge></li>
          </ul>
          <div class="col-span-2 min-[820px]:col-span-1">
            <BaseButton size="sm" :aria-label="`Open webhook ${w.name}`" @click="open(w)">Open</BaseButton>
          </div>
        </li>
      </ul>
      <div v-if="list.hasMore.value">
        <BaseButton :loading="list.loadingMore.value" @click="list.loadMore">Load more</BaseButton>
      </div>
      <BaseAlert v-if="list.error.value && list.items.value.length" :error="list.error.value" />

      <BaseCard title="Receiving webhooks" class="max-w-3xl">
        <p class="m-0 text-muted">
          Each event is an HTTPS <span class="lg-mono">POST</span> with a JSON body. Requests are not signed: authenticate them with HTTPS plus
          Basic or token authentication, a secret header, or a client certificate (mutual TLS). Delivery is at least once; deduplicate by event ID.
        </p>
        <dl class="m-0 flex flex-col">
          <div class="lg-prop"><dt class="lg-mono">X-Open-Kase-Event-Id</dt><dd>Stable across retries; use it to deduplicate.</dd></div>
          <div class="lg-prop"><dt class="lg-mono">X-Open-Kase-Event-Type</dt><dd>The event type, also in the body's <span class="lg-mono">type</span>.</dd></div>
        </dl>
        <a :href="DOCS_URL" target="_blank" rel="noopener noreferrer" class="lg-link inline-flex items-center gap-1 self-start">
          Full receiver guide (payloads, retries, authentication)
          <IconExternalLink :size="14" stroke="1.75" aria-hidden="true" />
        </a>
      </BaseCard>
    </template>

    <!-- New webhook -->
    <BaseModal v-model:open="creating" :title="createdHook ? 'Webhook created' : 'New webhook'" width="lg" @close="createError = null">
      <template v-if="createdHook">
        <p class="m-0">
          <span class="font-medium">{{ createdHook.name }}</span> is active and will receive the events you chose. Send a test delivery to check that the
          receiver answers.
        </p>
        <BaseAlert v-if="createdTestError" :error="createdTestError" />
        <DeliveryResult v-if="createdTest.delivery.value" :delivery="createdTest.delivery.value" :polling="createdTest.polling.value" :gave-up="createdTest.gaveUp.value" />
        <BaseAlert v-if="createdTest.error.value" :error="createdTest.error.value" />
      </template>
      <template v-else>
        <ol class="m-0 flex list-none flex-wrap gap-x-4 gap-y-1 p-0 text-xs" aria-label="Steps">
          <li v-for="(s, i) in STEP_TITLES" :key="s" :aria-current="step === i + 1 ? 'step' : undefined" :class="step === i + 1 ? 'font-semibold text-text' : 'text-muted'">
            {{ i + 1 }} {{ s }}
          </li>
        </ol>
        <WebhookForm ref="createForm" :event-types="eventTypes" :can-grant-notes="session.isAdmin" :error="createError" :section="stepSection" />
      </template>
      <template #footer>
        <template v-if="createdHook">
          <BaseButton :loading="createdTestBusy" @click="sendCreatedTest">
            <template #icon><IconSend :size="16" stroke="1.75" aria-hidden="true" /></template>
            Send a test now
          </BaseButton>
          <BaseButton variant="primary" @click="openCreated">Open webhook</BaseButton>
        </template>
        <template v-else>
          <BaseButton @click="creating = false">Cancel</BaseButton>
          <BaseButton v-if="step > 1" @click="step = (step - 1) as 1 | 2">Back</BaseButton>
          <BaseButton v-if="step < 3" variant="primary" :disabled="nextDisabled" @click="step = (step + 1) as 2 | 3">Next</BaseButton>
          <BaseButton v-else variant="primary" :loading="createBusy" @click="create">Create webhook</BaseButton>
        </template>
      </template>
    </BaseModal>

    <BaseModal v-model:open="deleteOpen" title="Delete this webhook?" :description="selected ? `${selected.name} stops receiving events; pending deliveries are skipped.` : undefined">
      <BaseAlert v-if="deleteError" :error="deleteError" />
      <template #footer>
        <BaseButton @click="deleteOpen = false">Cancel</BaseButton>
        <BaseButton variant="danger" :loading="deleteBusy" @click="remove"><template #icon><IconTrash :size="16" stroke="1.75" aria-hidden="true" /></template>Delete webhook</BaseButton>
      </template>
    </BaseModal>
  </div>
</template>
