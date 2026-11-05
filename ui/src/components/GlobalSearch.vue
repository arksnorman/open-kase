<script setup lang="ts">
// Global full-text search palette (Ctrl/Cmd+K). Searches ticket subjects and the message bodies
// the caller may read (GET /search); the server authorizes before counting and
// snippeting. Combobox pattern: focus stays in the input, ↑/↓ move the active option
// (aria-activedescendant), Enter opens the ticket, Esc closes (native dialog).
import { ticketRef, ticketRoute } from '@/format'
import { IconChevronDown, IconSearch } from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, useId, watch } from 'vue'
import { useRouter } from 'vue-router'
import { searchApi } from '@/api/endpoints'
import { ApiError, isAbortError } from '@/api/errors'
import type { SearchHit, Snippet } from '@/api/types'
import { BaseAlert, BaseButton, BaseModal, HighlightedText, IconButton, KbdKey, SearchInput, StatusBadge, TriggerButton } from '@/components/base'
import { anySignal, queryScopeSignal } from '@/composables/useQuery'

/** `compact`: an icon button (narrow shell top bar) instead of the sidebar's search field. */
withDefaults(defineProps<{ compact?: boolean }>(), { compact: false })

const MAX_Q = 100
const router = useRouter()
const uid = useId()
const listId = `${uid}-list`
const optionId = (i: number) => `${uid}-opt-${i}`

const open = ref(false)
const query = ref('')
const input = ref<InstanceType<typeof SearchInput> | null>(null)

const hits = shallowRef<SearchHit[]>([])
const total = ref(0)
const capped = ref(false)
const cursor = ref<string | null>(null)
/** The query the shown results belong to ('' = nothing searched yet). */
const shownQuery = ref('')
const loading = ref(false)
const loadingMore = ref(false)
const error = ref<unknown>(null)
const active = ref(-1)

let ctrl: AbortController | null = null
let debounce: ReturnType<typeof setTimeout> | undefined

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)
const shortcutLabel = isMac ? '⌘K' : 'Ctrl K'

function reset() {
  ctrl?.abort()
  ctrl = null
  clearTimeout(debounce)
  hits.value = []
  total.value = 0
  capped.value = false
  cursor.value = null
  shownQuery.value = ''
  loading.value = false
  loadingMore.value = false
  error.value = null
  active.value = -1
}

async function run(q: string, more: boolean) {
  ctrl?.abort()
  const own = new AbortController()
  ctrl = own
  if (more) loadingMore.value = true
  else loading.value = true
  error.value = null
  try {
    const page = await searchApi.tickets(q, more ? cursor.value : null, { signal: anySignal([own.signal, queryScopeSignal()]) })
    if (own.signal.aborted) return
    hits.value = more ? [...hits.value, ...page.items] : page.items
    total.value = page.total
    capped.value = page.totalCapped
    cursor.value = page.nextCursor
    shownQuery.value = q
    if (!more) active.value = page.items.length ? 0 : -1
  } catch (err) {
    if (isAbortError(err) || own.signal.aborted) return
    error.value = err instanceof ApiError && err.status === 422 ? 'Enter 1 to 100 characters to search.' : err
  } finally {
    if (ctrl === own) {
      loading.value = false
      loadingMore.value = false
    }
  }
}

watch(query, (v) => {
  clearTimeout(debounce)
  const q = v.trim()
  if (!q) return reset()
  if (q.length > MAX_Q) {
    error.value = `Search words can be at most ${MAX_Q} characters.`
    return
  }
  debounce = setTimeout(() => void run(q, false), 250)
})

function loadMore() {
  if (cursor.value && shownQuery.value && !loadingMore.value) void run(shownQuery.value, true)
}

function openHit(hit: SearchHit | undefined) {
  if (!hit) return
  open.value = false
  void router.push(ticketRoute(hit.ticket))
}

function scrollActive() {
  void nextTick(() => document.getElementById(optionId(active.value))?.scrollIntoView({ block: 'nearest' }))
}

function onKey(e: KeyboardEvent) {
  const n = hits.value.length
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault()
      if (!n) return
      if (active.value === n - 1 && cursor.value) loadMore()
      active.value = Math.min(n - 1, active.value + 1)
      scrollActive()
      break
    case 'ArrowUp':
      e.preventDefault()
      if (!n) return
      active.value = Math.max(0, active.value - 1)
      scrollActive()
      break
    case 'Home':
    case 'End':
      if (!n || !e.ctrlKey) return
      e.preventDefault()
      active.value = e.key === 'Home' ? 0 : n - 1
      scrollActive()
      break
    case 'Enter':
      e.preventDefault()
      clearTimeout(debounce)
      // Enter before the debounce fired: search now instead of opening a stale result.
      if (query.value.trim() && query.value.trim() !== shownQuery.value) void run(query.value.trim(), false)
      else openHit(hits.value[active.value])
      break
  }
}

function onGlobalKey(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    open.value = !open.value
  }
}
onMounted(() => document.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onGlobalKey)
  reset()
})

watch(open, async (v) => {
  if (!v) return
  await nextTick()
  input.value?.focus()
  input.value?.select()
})

const status = computed(() => {
  if (!query.value.trim()) return 'Type to search subjects and the messages you can read.'
  if (loading.value && !shownQuery.value) return 'Searching…'
  if (!shownQuery.value) return ''
  if (!total.value) return `No tickets match “${shownQuery.value}”.`
  const n = capped.value ? `At least ${total.value.toLocaleString()}` : total.value.toLocaleString()
  return `${n} ${total.value === 1 && !capped.value ? 'ticket matches' : 'tickets match'} “${shownQuery.value}”${hits.value.length < total.value ? ` · showing ${hits.value.length}` : ''}`
})

function subjectSnippet(hit: SearchHit): Snippet | undefined {
  return hit.snippets.find((s) => s.field === 'subject')
}
function messageSnippets(hit: SearchHit): Snippet[] {
  return hit.snippets.filter((s) => s.field === 'message').slice(0, 2)
}
function snippetLabel(s: Snippet): string {
  return s.visibility === 'agent' ? 'Agent note' : 'Public message'
}
</script>

<template>
  <IconButton
    v-if="compact"
    label="Search tickets"
    aria-haspopup="dialog"
    :aria-keyshortcuts="isMac ? 'Meta+K' : 'Control+K'"
    @click="open = true"
  >
    <IconSearch :size="18" stroke="1.75" />
  </IconButton>
  <TriggerButton
    v-else
    size="sm"
    aria-haspopup="dialog"
    :aria-keyshortcuts="isMac ? 'Meta+K' : 'Control+K'"
    @click="open = true"
  >
    <IconSearch :size="16" stroke="1.75" aria-hidden="true" />
    <span class="flex-1 text-left">Search</span>
    <KbdKey aria-hidden="true">{{ shortcutLabel }}</KbdKey>
  </TriggerButton>

  <BaseModal v-model:open="open" title="Search tickets" width="lg">
    <div class="flex flex-col gap-2">
      <SearchInput
          ref="input"
          v-model="query"
          type="text"
          size="lg"
          label="Search tickets"
          :loading="loading"
          role="combobox"
          autofocus
          autocomplete="off"
          spellcheck="false"
          aria-autocomplete="list"
          :aria-expanded="hits.length > 0"
          :aria-controls="listId"
          :aria-activedescendant="active >= 0 && hits[active] ? optionId(active) : undefined"
          :aria-describedby="`${uid}-status`"
          :maxlength="MAX_Q + 20"
          placeholder="Words, &quot;a phrase&quot;, -exclude"
          @keydown="onKey"
        />

      <p :id="`${uid}-status`" class="m-0 text-xs text-muted" aria-live="polite">{{ status }}</p>
      <BaseAlert v-if="error" :error="error" />

      <ul
        v-show="hits.length"
        :id="listId"
        role="listbox"
        aria-label="Search results"
        class="m-0 flex max-h-[min(460px,60vh)] list-none flex-col overflow-auto rounded-md border border-border p-0"
      >
        <li
          v-for="(hit, i) in hits"
          :id="optionId(i)"
          :key="hit.ticket.id"
          role="option"
          :aria-selected="i === active"
          class="flex cursor-pointer flex-col gap-1 border-b border-line px-3 py-2 last:border-b-0"
          :class="i === active ? 'bg-sel' : ''"
          @mousemove="active = i"
          @click="openHit(hit)"
        >
          <div class="flex min-w-0 items-center gap-2">
            <span class="lg-mono flex-none text-xs text-muted">{{ ticketRef(hit.ticket) }}</span>
            <span class="lg-ell min-w-0 flex-1 font-medium">
              <HighlightedText v-if="subjectSnippet(hit)" :text="subjectSnippet(hit)!.text" :ranges="subjectSnippet(hit)!.highlights" />
              <template v-else>{{ hit.ticket.subject }}</template>
            </span>
            <StatusBadge :status="hit.ticket.status" class="flex-none" />
          </div>
          <p v-for="(s, j) in messageSnippets(hit)" :key="s.messageId ?? j" class="m-0 line-clamp-2 text-xs text-muted">
            <span class="font-medium">{{ snippetLabel(s) }}:</span>{{ ' ' }}<HighlightedText :text="s.text" :ranges="s.highlights" />
          </p>
        </li>
      </ul>

      <div class="flex items-center justify-between gap-2 text-xs text-muted">
        <span><KbdKey>↑</KbdKey> <KbdKey>↓</KbdKey> move · <KbdKey>Enter</KbdKey> open · <KbdKey>Esc</KbdKey> close</span>
        <BaseButton v-if="cursor" size="sm" :loading="loadingMore" @click="loadMore"><template #icon><IconChevronDown :size="14" stroke="1.75" aria-hidden="true" /></template>Load more</BaseButton>
      </div>
    </div>
  </BaseModal>
</template>
