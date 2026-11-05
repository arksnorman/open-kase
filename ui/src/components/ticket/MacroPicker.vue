<script setup lang="ts">
// "Insert canned reply": pick a canned reply, preview its rendered body for this ticket, then insert
// it into the composer of its audience. Nothing is sent from here.
import { ticketRef } from '@/format'
import { IconLock, IconWorld } from '@tabler/icons-vue'
import { computed, ref, watch } from 'vue'
import { macrosApi } from '@/api/endpoints'
import { isAbortError } from '@/api/errors'
import type { MacroPreview, MessageVisibility, Ticket } from '@/api/types'
import { BaseAlert, BaseBadge, BaseButton, BaseCard, BaseModal, LoadingState, MarkdownView, SearchInput, SelectableRow } from '@/components/base'
import { useQuery } from '@/composables/useQuery'

const props = defineProps<{ ticket: Ticket }>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ insert: [body: string, visibility: MessageVisibility] }>()

const macrosQ = useQuery(
  () => (open.value ? `p:${props.ticket.projectId}:macros` : null),
  (signal) => macrosApi.list({ signal }),
)
const search = ref('')
const macros = computed(() => {
  const q = search.value.trim().toLowerCase()
  return (macrosQ.data.value?.items ?? [])
    .filter((m) => m.active && (!q || m.name.toLowerCase().includes(q)))
    .sort((a, b) => a.name.localeCompare(b.name))
})

const selectedId = ref<string | null>(null)
const preview = ref<MacroPreview | null>(null)
const previewLoading = ref(false)
const error = ref<unknown>(null)
let ctrl: AbortController | null = null

watch(open, (v) => {
  if (!v) return
  search.value = ''
  selectedId.value = null
  preview.value = null
  error.value = null
})

async function loadPreview(id: string) {
  ctrl?.abort()
  const own = new AbortController()
  ctrl = own
  selectedId.value = id
  preview.value = null
  error.value = null
  previewLoading.value = true
  try {
    const p = await macrosApi.preview(props.ticket.id, id, { signal: own.signal })
    if (ctrl === own) preview.value = p
  } catch (err) {
    if (!isAbortError(err) && ctrl === own) error.value = err
  } finally {
    if (ctrl === own) previewLoading.value = false
  }
}

const selected = computed(() => macros.value.find((m) => m.id === selectedId.value) ?? macrosQ.data.value?.items.find((m) => m.id === selectedId.value))
const body = computed(() => preview.value?.body ?? '')
const visibility = computed<MessageVisibility>(() => preview.value?.visibility ?? 'public')

function insert() {
  if (!body.value.trim()) return
  emit('insert', body.value, visibility.value)
  open.value = false
}

// ↑/↓/Home/End move between canned replies and preview the one reached (SearchInput ↓ enters the list).
function onListKey(e: KeyboardEvent) {
  const rows = Array.from((e.currentTarget as HTMLElement).querySelectorAll<HTMLButtonElement>('li > button'))
  const at = rows.indexOf(document.activeElement as HTMLButtonElement)
  const next = e.key === 'ArrowDown' ? at + 1 : e.key === 'ArrowUp' ? at - 1 : e.key === 'Home' ? 0 : e.key === 'End' ? rows.length - 1 : null
  if (next === null) return
  e.preventDefault()
  const row = rows[Math.max(0, Math.min(rows.length - 1, next))]
  row?.focus()
  row?.click()
}
function enterList(e: KeyboardEvent) {
  const first = (e.currentTarget as HTMLElement).closest('div')?.parentElement?.querySelector<HTMLButtonElement>('ul li > button')
  if (!first) return
  e.preventDefault()
  first.focus()
  first.click()
}

function audienceLabel(v: MessageVisibility) {
  return v === 'public' ? 'Public reply' : 'Agent note'
}
</script>

<template>
  <BaseModal v-model:open="open" width="lg" title="Canned replies" description="Insert puts the reply into the composer for review; nothing is sent.">
    <!-- Side by side from 640 px; on a phone the list sits above the preview. -->
    <div class="grid min-h-[320px] grid-cols-1 gap-4 sm:grid-cols-[220px_1fr]">
      <div class="flex min-h-0 flex-col gap-2">
        <SearchInput id="macro-search" v-model="search" label="Search canned replies" placeholder="Search" @keydown.down="enterList" />
        <LoadingState v-if="macrosQ.loading.value && !macrosQ.data.value" size="sm" />
        <BaseAlert v-else-if="macrosQ.error.value" :error="macrosQ.error.value" />
        <p v-else-if="!macros.length" class="m-0 text-muted">{{ search ? 'No canned replies match.' : 'No active canned replies in this project.' }}</p>
        <ul v-else class="m-0 flex max-h-[380px] list-none flex-col gap-px overflow-auto p-0" aria-label="Canned replies" @keydown="onListKey">
          <li v-for="m in macros" :key="m.id">
            <SelectableRow variant="plain" :selected="selectedId === m.id" :aria-current="selectedId === m.id ? 'true' : undefined" @click="loadPreview(m.id)">
              <span class="lg-ell flex-1">{{ m.name }}</span>
              <span class="text-xs font-normal text-muted">{{ m.visibility === 'agent' ? 'Note' : 'Reply' }}</span>
            </SelectableRow>
          </li>
        </ul>
      </div>

      <div class="flex min-w-0 flex-col gap-3" aria-live="polite">
        <p v-if="!selectedId" class="m-0 text-muted">Choose a canned reply to preview it for {{ ticketRef(ticket) }}.</p>
        <LoadingState v-else-if="previewLoading" text="Loading preview…" size="sm" />
        <BaseAlert v-if="error" :error="error" />
        <template v-if="preview && !previewLoading">
          <div class="flex flex-wrap items-center gap-2">
            <h3 class="m-0 text-[14px] font-semibold">{{ selected?.name }}</h3>
            <BaseBadge :tone="visibility === 'agent' ? 'note' : 'neutral'">
              <template #icon>
                <IconLock v-if="visibility === 'agent'" :size="12" stroke="1.75" aria-hidden="true" />
                <IconWorld v-else :size="12" stroke="1.75" aria-hidden="true" />
              </template>
              For: {{ audienceLabel(visibility) }}
            </BaseBadge>
          </div>
          <section aria-label="Reply preview" class="flex flex-col gap-1.5">
            <h4 class="lg-label m-0">Reply</h4>
            <BaseCard as="div" size="sm" :tone="visibility === 'agent' ? 'note' : 'default'" class="max-h-56 overflow-auto" tabindex="0">
              <MarkdownView :source="body" />
            </BaseCard>
          </section>
        </template>
      </div>
    </div>
    <template #footer>
      <BaseButton @click="open = false">Close</BaseButton>
      <BaseButton variant="primary" :disabled="!preview || previewLoading || !body.trim()" @click="insert">
        Insert into {{ audienceLabel(visibility).toLowerCase() }}
      </BaseButton>
    </template>
  </BaseModal>
</template>
