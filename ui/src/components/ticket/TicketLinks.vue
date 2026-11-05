<script setup lang="ts">
// Linked tickets (GET/POST/DELETE .../links). Each side is authorized separately: links to
// tickets the agent cannot read are not listed at all. Archived tickets can be linked.
import { ticketRef, ticketRoute } from '@/format'
import { IconLink, IconX } from '@tabler/icons-vue'
import { computed, ref, useId } from 'vue'
import { ticketsApi, workflowApi } from '@/api/endpoints'
import { ApiError } from '@/api/errors'
import type { LinkRelation, LinkType, Ticket } from '@/api/types'
import {
  BaseAlert,
  BaseButton,
  BaseModal,
  FormField,
  IconButton,
  LoadingState,
  SelectInput,
  Spinner,
  StatusBadge,
  type SelectOption,
} from '@/components/base'
import { anySignal, useQuery, queryScopeSignal } from '@/composables/useQuery'

const props = defineProps<{ ticket: Ticket; /** Cache key of the open ticket. */ queryBase: string }>()
const emit = defineEmits<{ changed: [] }>()
const uid = useId()

const linksQ = useQuery(
  () => `${props.queryBase}:links`,
  (signal) => workflowApi.links(props.ticket.id, { signal }),
)
const links = computed(() => linksQ.data.value?.items ?? [])

const RELATION: Record<LinkRelation, string> = {
  relatedTo: 'Related to',
  duplicateOf: 'Duplicate of',
  duplicatedBy: 'Duplicated by',
  followUpTo: 'Follow-up to',
  followedUpBy: 'Followed up by',
  copiedFrom: 'Copy of',
  copiedTo: 'Copied to',
}

const busy = ref<string | null>(null)
const error = ref<unknown>(null)
async function remove(id: string) {
  busy.value = id
  error.value = null
  try {
    await workflowApi.removeLink(props.ticket.id, id)
    emit('changed')
  } catch (err) {
    error.value = err instanceof ApiError && err.is('linkNotFound') ? 'That link no longer exists. The list has been refreshed.' : err
  } finally {
    busy.value = null
    await linksQ.refetch()
  }
}

// ---- add ----
const addOpen = ref(false)
const type = ref<LinkType | ''>('relatedTo')
const typeOptions: { value: LinkType; label: string }[] = [
  { value: 'relatedTo', label: 'Related to' },
  { value: 'duplicateOf', label: 'Duplicate of' },
  { value: 'followUpTo', label: 'Follow-up to' },
]
const chosenId = ref('')
const found = new Map<string, Ticket>()
const chosen = computed(() => (chosenId.value ? (found.get(chosenId.value) ?? null) : null))
const addError = ref<unknown>(null)
const adding = ref(false)

function openAdd() {
  type.value = 'relatedTo'
  chosenId.value = ''
  addError.value = null
  addOpen.value = true
}

// Async options for the ticket picker (SelectInput debounces and aborts superseded searches).
async function searchTickets(text: string, signal: AbortSignal): Promise<SelectOption<string>[]> {
  const page = await ticketsApi.list(
    { view: 'all', q: text.slice(0, 100), limit: 8 },
    { signal: anySignal([signal, queryScopeSignal()]) },
  )
  const items = page.items.filter((t) => t.id !== props.ticket.id)
  for (const t of items) found.set(t.id, t)
  return items.map((t) => ({ value: t.id, label: `${ticketRef(t)} ${t.subject}` }))
}
const linkedIds = computed(() => new Set(links.value.map((l) => l.ticket.id)))

async function add() {
  if (!chosen.value || !type.value) return
  adding.value = true
  addError.value = null
  try {
    await workflowApi.addLink(props.ticket.id, { type: type.value, ticket: chosen.value.id })
    addOpen.value = false
    emit('changed')
    await linksQ.refetch()
  } catch (err) {
    if (err instanceof ApiError && err.is('linkExists')) addError.value = `${ticketRef(chosen.value)} is already linked this way.`
    else if (err instanceof ApiError && err.is('tooManyLinks')) addError.value = 'A ticket can have at most 100 links.'
    else if (err instanceof ApiError && err.status === 422 && err.field('ticket'))
      addError.value = 'That ticket cannot be linked (it may have been deleted or is not visible to you).'
    else addError.value = err
  } finally {
    adding.value = false
  }
}
</script>

<template>
  <dt :id="`${uid}-h`" class="self-start text-muted">Links</dt>
  <dd class="m-0 flex min-w-0 flex-col items-start gap-1.5" :aria-labelledby="`${uid}-h`">
    <LoadingState v-if="linksQ.loading.value && !linksQ.data.value" text="Loading links…" size="sm" />
    <BaseAlert v-else-if="linksQ.error.value" :error="linksQ.error.value" />
    <span v-else-if="!links.length" class="text-muted">None</span>
    <ul v-else class="m-0 flex w-full list-none flex-col gap-1.5 p-0">
      <li v-for="l in links" :key="l.id" class="flex items-start gap-2 border-t border-line pt-1.5">
        <div class="flex min-w-0 flex-1 flex-col gap-0.5">
          <span class="text-xs text-muted">{{ RELATION[l.relation] }}</span>
          <RouterLink :to="ticketRoute(l.ticket)" class="lg-link flex min-w-0 items-baseline gap-1.5">
            <span class="lg-mono flex-none text-xs">{{ ticketRef(l.ticket) }}</span>
            <span class="lg-ell">{{ l.ticket.subject }}</span>
          </RouterLink>
          <span><StatusBadge :status="l.ticket.status" /></span>
        </div>
        <IconButton :label="`Remove link to ${ticketRef(l.ticket)}`" :disabled="busy !== null" @click="remove(l.id)">
          <Spinner v-if="busy === l.id" :size="12" />
          <IconX v-else :size="14" stroke="1.75" />
        </IconButton>
      </li>
    </ul>
    <BaseAlert v-if="error" :error="error" />
    <div class="flex flex-wrap items-center gap-1.5">
      <BaseButton size="sm" variant="ghost" aria-haspopup="dialog" @click="openAdd">
        <template #icon><IconLink :size="14" stroke="1.75" aria-hidden="true" /></template>
        Link ticket…
      </BaseButton>
      <slot />
    </div>

    <BaseModal v-model:open="addOpen" title="Link ticket" width="md" :description="`Link ${ticketRef(ticket)} to another ticket of this project. Links are visible to staff who can read both tickets.`">
      <form class="flex flex-col gap-3" novalidate @submit.prevent="add">
        <BaseAlert v-if="addError" :error="addError" />
        <FormField :label="`${ticketRef(ticket)} is`">
          <SelectInput v-model="type" :options="typeOptions" />
        </FormField>
        <FormField label="Ticket" hint="Search by ticket number (1042 or #1042) or subject.">
          <SelectInput
            v-model="chosenId"
            :loader="searchTickets"
            :min-query="1"
            idle-text="Type a ticket number or subject"
            empty-text="No matching tickets"
            search-placeholder="Ticket number or subject"
            placeholder="Choose a ticket"
            label="Matching tickets"
            panel-class="w-[min(520px,calc(100vw-32px))]"
          >
            <template #value>
              <template v-if="chosen">
                <span class="lg-mono flex-none text-xs">{{ ticketRef(chosen) }}</span>
                <span class="lg-ell min-w-0 flex-1">{{ chosen.subject }}</span>
              </template>
            </template>
            <template #option="{ option }">
              <template v-if="found.get(option.value)">
                <span class="lg-mono flex-none text-xs">{{ ticketRef(found.get(option.value)!) }}</span>
                <span class="lg-ell min-w-0 flex-1 font-normal">{{ found.get(option.value)!.subject }}</span>
                <span v-if="linkedIds.has(option.value)" class="flex-none text-xs font-normal text-muted">linked</span>
                <StatusBadge :status="found.get(option.value)!.status" />
              </template>
            </template>
          </SelectInput>
        </FormField>
        <div class="flex justify-end gap-2">
          <BaseButton @click="addOpen = false">Cancel</BaseButton>
          <BaseButton type="submit" variant="primary" :loading="adding" :disabled="!chosen || !type">
            {{ chosen ? `Link ${ticketRef(chosen)}` : 'Link ticket' }}
          </BaseButton>
        </div>
      </form>
    </BaseModal>
  </dd>
</template>
