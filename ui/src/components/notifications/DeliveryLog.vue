<script setup lang="ts">
// Filterable, paginated delivery table with details and manual retry. Used by the
// installation-wide delivery log and by each webhook endpoint.
import { IconRefresh, IconSend } from '@tabler/icons-vue'
import { computed, onScopeDispose, ref, shallowRef, watch } from 'vue'
import { ApiError, isAbortError } from '@/api/errors'
import type { Delivery, DeliveryChannel, DeliveryFilters, DeliveryStatus, Page } from '@/api/types'
import {
  BaseAlert,
  BaseAvatar,
  BaseBadge,
  BaseButton,
  BaseModal,
  DataTable,
  FormField,
  RelativeTime,
  SelectInput,
  TextInput,
  type Column,
  type SelectOption,
} from '@/components/base'
import {
  DELIVERY_STATUSES,
  deliveryAvatar,
  deliveryRecipient, deliveryRecipientTitle,
  deliveryStatusExplanation,
  deliveryStatusLabel,
  deliveryStatusTone,
  errorClassExplanation,
  isRetryable,
} from '@/deliveries'
import { eventName } from '@/webhookLabels'
import DeliveryStatusBadge from './DeliveryStatusBadge.vue'

const props = withDefaults(
  defineProps<{
    label: string
    fetchPage: (q: DeliveryFilters & { cursor: string | null; limit: number }, signal: AbortSignal) => Promise<Page<Delivery>>
    retry: (d: Delivery) => Promise<Delivery>
    /** Shows the channel column and filter (installation log). */
    showChannel?: boolean
    /** Changing this reloads the list (e.g. a different endpoint). */
    reloadKey?: string
    /** `endpoint`: the compact table of one webhook (time, event, ticket, result, tries). */
    variant?: 'log' | 'endpoint'
  }>(),
  { showChannel: false, reloadKey: '', variant: 'log' },
)

const channel = ref<DeliveryChannel | ''>('')
const status = ref<DeliveryStatus | ''>('')
const eventId = ref('')
const appliedEventId = ref('')

const channelOptions: SelectOption<DeliveryChannel>[] = [
  { value: 'email', label: 'Email' },
  { value: 'webhook', label: 'Webhook' },
]
const statusOptions: SelectOption<DeliveryStatus>[] = DELIVERY_STATUSES.map((s) => ({ value: s, label: deliveryStatusLabel(s) }))

const columns = computed<Column[]>(() =>
  props.variant === 'endpoint'
    ? [
        { key: 'createdAt', label: 'Time', width: '110px' },
        { key: 'eventType', label: 'Event' },
        { key: 'ticket', label: 'Ticket', width: '84px' },
        { key: 'status', label: 'Result' },
        { key: 'attempts', label: 'Tries', width: '64px' },
        { key: 'actions', label: 'Actions', width: '150px' },
      ]
    : logColumns.value,
)
const logColumns = computed<Column[]>(() => [
  ...(props.showChannel ? [{ key: 'channel', label: 'Channel', width: '84px' }] : []),
  { key: 'recipient', label: props.showChannel ? 'Recipient / endpoint' : 'Endpoint' },
  { key: 'eventType', label: 'Event' },
  { key: 'status', label: 'Status', width: '138px' },
  { key: 'attempts', label: 'Attempts', width: '78px' },
  { key: 'nextAttemptAt', label: 'Next retry', width: '104px' },
  { key: 'errorClass', label: 'Error class' },
  { key: 'createdAt', label: 'Created', width: '104px' },
  { key: 'actions', label: 'Actions', width: '140px' },
])

const rows = shallowRef<Delivery[]>([])
const cursor = ref<string | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
let ctrl: AbortController | null = null

async function load(append: boolean): Promise<void> {
  ctrl?.abort()
  const own = new AbortController()
  ctrl = own
  loading.value = true
  error.value = null
  try {
    const page = await props.fetchPage(
      {
        channel: props.showChannel ? channel.value : '',
        status: status.value,
        eventId: appliedEventId.value.trim(),
        cursor: append ? cursor.value : null,
        limit: 50,
      },
      own.signal,
    )
    if (own.signal.aborted) return
    rows.value = append ? [...rows.value, ...page.items] : page.items
    cursor.value = page.nextCursor
  } catch (err) {
    if (!isAbortError(err) && !own.signal.aborted) error.value = err
  } finally {
    if (ctrl === own) loading.value = false
  }
}

watch([channel, status, appliedEventId, () => props.reloadKey], () => void load(false), { immediate: true })
onScopeDispose(() => ctrl?.abort())

function applyEventId() {
  appliedEventId.value = eventId.value
}

const eventIdError = computed(() => (error.value instanceof ApiError ? error.value.field('eventId') : undefined))
/** A load failure other than a bad event ID (which is shown on the field). */
const listError = computed(() => (eventIdError.value ? null : error.value))
const filtered = computed(() => !!((props.showChannel && channel.value) || status.value || appliedEventId.value.trim()))
function clearFilters() {
  channel.value = ''
  status.value = ''
  eventId.value = ''
  appliedEventId.value = ''
}

function replaceRow(d: Delivery) {
  rows.value = rows.value.map((r) => (r.id === d.id ? d : r))
  if (detail.value?.id === d.id) detail.value = d
}

// Details
const detail = shallowRef<Delivery | null>(null)
const detailOpen = computed({
  get: () => detail.value !== null,
  set: (v: boolean) => {
    if (!v) detail.value = null
  },
})

// Retry (unknownOutcome asks for confirmation because it may duplicate the email)
const retryTarget = shallowRef<Delivery | null>(null)
const retryBusy = ref<string | null>(null)
const retryError = ref<unknown>(null)
const confirmOpen = computed({
  get: () => retryTarget.value !== null,
  set: (v: boolean) => {
    if (!v) retryTarget.value = null
  },
})

function askRetry(d: Delivery) {
  retryError.value = null
  if (d.status === 'unknownOutcome') {
    detail.value = null
    retryTarget.value = d
  } else void doRetry(d)
}

async function doRetry(d: Delivery) {
  retryBusy.value = d.id
  retryError.value = null
  try {
    replaceRow(await props.retry(d))
    retryTarget.value = null
  } catch (err) {
    retryError.value = err
    if (err instanceof ApiError && err.is('deliveryNotRetryable')) void load(false)
  } finally {
    retryBusy.value = null
  }
}
</script>

<template>
  <div class="flex flex-col gap-3">
    <div class="flex flex-wrap items-end gap-3">
      <div v-if="showChannel" class="w-36">
        <FormField label="Channel"><SelectInput v-model="channel" :options="channelOptions" size="sm" placeholder="All channels" clearable /></FormField>
      </div>
      <div class="w-44">
        <FormField label="Status"><SelectInput v-model="status" :options="statusOptions" size="sm" placeholder="All statuses" clearable /></FormField>
      </div>
      <form class="flex items-end gap-2" @submit.prevent="applyEventId">
        <div class="w-64">
          <FormField label="Event ID" :error="eventIdError"><TextInput v-model="eventId" size="sm" mono autocomplete="off" @blur="applyEventId" /></FormField>
        </div>
      </form>
      <div class="flex-1"></div>
      <BaseButton size="sm" :loading="loading" @click="load(false)">
        <template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>
        Refresh
      </BaseButton>
    </div>

    <BaseAlert v-if="retryError && !confirmOpen" :error="retryError" />

    <DataTable
      :columns="columns"
      :rows="rows"
      :row-key="(d: Delivery) => d.id"
      :label="label"
      :loading="loading"
      :error="listError"
      :filtered="filtered"
      filtered-text="No deliveries match these filters."
      empty-text="No deliveries yet."
      :empty-icon="IconSend"
      :has-more="!!cursor"
      :loading-more="loading && rows.length > 0"
      @retry="load(false)"
      @clear-filters="clearFilters"
      @load-more="load(true)"
    >
      <template #cell-channel="{ row }"><span class="capitalize">{{ row.channel }}</span></template>
      <template #cell-recipient="{ row }">
        <div class="flex items-center gap-2">
          <BaseAvatar :name="deliveryRecipient(row)" :role="deliveryAvatar(row)" size="xs" :decorative="!showChannel" />
          <div class="min-w-0">
            <div class="lg-ell text-xs" :class="row.recipient.name && row.channel === 'webhook' ? 'font-medium' : 'lg-mono'" v-tooltip.truncated="deliveryRecipientTitle(row)">{{ deliveryRecipient(row) }}</div>
            <div class="text-[11px] text-muted">{{ row.reason.replace('_', ' ') }}</div>
          </div>
        </div>
      </template>
      <template #cell-eventType="{ row }">
        <template v-if="variant === 'endpoint'">
          <span class="block text-[13px]">{{ eventName(row.eventType) }}</span>
          <span class="lg-mono lg-ell block text-[11px] text-muted" v-tooltip.truncated="row.eventType">{{ row.eventType }}</span>
        </template>
        <span v-else class="lg-mono lg-ell block text-xs" v-tooltip.truncated="row.eventType">{{ row.eventType }}</span>
      </template>
      <template #cell-ticket="{ row }">
        <RouterLink v-if="row.ticketNumber" :to="{ name: 'ticket', params: { ticketNumber: row.ticketNumber } }" class="lg-link lg-mono text-xs">#{{ row.ticketNumber }}</RouterLink>
        <span v-else class="text-muted">—</span>
      </template>
      <template #cell-status="{ row }">
        <div v-if="variant === 'endpoint'" class="flex flex-wrap items-center gap-1.5">
          <BaseBadge :tone="deliveryStatusTone(row.status)" v-tooltip="deliveryStatusExplanation(row.status)">{{ row.status === 'sent' ? 'Delivered' : deliveryStatusLabel(row.status) }}</BaseBadge>
          <span v-if="row.statusCode" class="lg-mono text-xs">HTTP {{ row.statusCode }}<template v-if="row.responseMs"> · {{ row.responseMs }} ms</template></span>
          <span v-else-if="row.errorClass" class="lg-mono text-xs" v-tooltip="errorClassExplanation(row.errorClass) ?? row.error ?? ''">{{ row.errorClass }}</span>
          <span v-if="row.nextAttemptAt && row.status === 'retrying'" class="text-[11px] text-muted">next try <RelativeTime :iso="row.nextAttemptAt" /></span>
        </div>
        <DeliveryStatusBadge v-else :status="row.status" />
      </template>
      <template #cell-attempts="{ row }"><span class="lg-mono text-xs">{{ row.attempts }}</span></template>
      <template #cell-nextAttemptAt="{ row }"><RelativeTime v-if="row.nextAttemptAt" :iso="row.nextAttemptAt" /><span v-else class="text-muted">—</span></template>
      <template #cell-errorClass="{ row }">
        <span v-if="row.errorClass" class="lg-mono lg-ell block text-xs" v-tooltip.truncated="errorClassExplanation(row.errorClass) ?? row.error ?? ''">{{ row.errorClass }}</span>
        <span v-else class="text-muted">—</span>
      </template>
      <template #cell-createdAt="{ row }"><RelativeTime :iso="row.createdAt" /></template>
      <template #cell-actions="{ row }">
        <div class="flex gap-1.5">
          <BaseButton size="sm" variant="ghost" @click="detail = row">Details</BaseButton>
          <BaseButton v-if="isRetryable(row)" size="sm" :loading="retryBusy === row.id" @click="askRetry(row)"><template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>Retry</BaseButton>
        </div>
      </template>
    </DataTable>


    <BaseModal v-model:open="detailOpen" width="md" title="Delivery details" :description="detail ? deliveryStatusExplanation(detail.status) : undefined">
      <dl v-if="detail" class="m-0 flex flex-col">
        <div class="lg-prop"><dt>Status</dt><dd><DeliveryStatusBadge :status="detail.status" /></dd></div>
        <div class="lg-prop"><dt>Channel</dt><dd class="capitalize">{{ detail.channel }}</dd></div>
        <div class="lg-prop"><dt>Recipient</dt><dd class="flex items-start gap-2"><BaseAvatar :name="deliveryRecipient(detail)" :role="deliveryAvatar(detail)" size="xs" :decorative="!showChannel" /><span class="lg-mono text-xs break-all">{{ deliveryRecipient(detail) }} ({{ detail.reason.replace('_', ' ') }})</span></dd></div>
        <div class="lg-prop"><dt>Event</dt><dd class="lg-mono text-xs break-all">{{ detail.eventType }}<template v-if="detail.eventId"> · {{ detail.eventId }}</template></dd></div>
        <div v-if="detail.template" class="lg-prop"><dt>Template</dt><dd class="lg-mono text-xs">{{ detail.template }}</dd></div>
        <div class="lg-prop"><dt>Attempts</dt><dd>{{ detail.attempts }}</dd></div>
        <div v-if="detail.lastAttemptAt" class="lg-prop"><dt>Last attempt</dt><dd><RelativeTime :iso="detail.lastAttemptAt" /></dd></div>
        <div v-if="detail.nextAttemptAt" class="lg-prop"><dt>Next retry</dt><dd><RelativeTime :iso="detail.nextAttemptAt" /></dd></div>
        <div v-if="detail.completedAt" class="lg-prop"><dt>Completed</dt><dd><RelativeTime :iso="detail.completedAt" /></dd></div>
        <div class="lg-prop"><dt>Created</dt><dd><RelativeTime :iso="detail.createdAt" /></dd></div>
        <div v-if="detail.errorClass" class="lg-prop">
          <dt>Error class</dt>
          <dd><span class="lg-mono text-xs">{{ detail.errorClass }}</span><span v-if="errorClassExplanation(detail.errorClass)" class="text-muted"> — {{ errorClassExplanation(detail.errorClass) }}</span></dd>
        </div>
        <div v-if="detail.error" class="lg-prop"><dt>Error</dt><dd class="lg-mono text-xs break-all">{{ detail.error }}</dd></div>
        <div v-if="detail.statusCode" class="lg-prop"><dt>HTTP status</dt><dd class="lg-mono text-xs">{{ detail.statusCode }}</dd></div>
        <div v-if="detail.responseSnippet" class="lg-prop"><dt>Response</dt><dd class="lg-mono text-xs break-all whitespace-pre-wrap">{{ detail.responseSnippet }}</dd></div>
        <div v-if="detail.messageId" class="lg-prop"><dt>Message-ID</dt><dd class="lg-mono text-xs break-all">{{ detail.messageId }}</dd></div>
      </dl>
      <template #footer>
        <BaseButton v-if="detail && isRetryable(detail)" :loading="retryBusy === detail.id" @click="askRetry(detail)"><template #icon><IconRefresh :size="16" stroke="1.75" aria-hidden="true" /></template>Retry</BaseButton>
        <BaseButton variant="primary" @click="detail = null">Close</BaseButton>
      </template>
    </BaseModal>

    <BaseModal
      v-model:open="confirmOpen"
      title="Retry an uncertain delivery?"
      description="The mail server may already have accepted this message. Retrying sends it again with the same Message-ID, which many mail systems collapse, but the recipient may still get a duplicate."
    >
      <BaseAlert v-if="retryError" :error="retryError" />
      <template #footer>
        <BaseButton @click="retryTarget = null">Cancel</BaseButton>
        <BaseButton variant="primary" :loading="retryBusy !== null" @click="retryTarget && doRetry(retryTarget)"><template #icon><IconRefresh :size="16" stroke="1.75" aria-hidden="true" /></template>Retry anyway</BaseButton>
      </template>
    </BaseModal>
  </div>
</template>
