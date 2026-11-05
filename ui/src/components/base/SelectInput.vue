<script setup lang="ts" generic="V extends string | number, Multi extends boolean = false">
// The one select of the app: single or multiple, optionally searchable, clearable, grouped,
// async (loader), creatable, with custom option / value / chip / trigger slots.
//
// ARIA (WAI-ARIA APG):
// - not searchable: the trigger is a "select-only combobox" (role=combobox, focus stays on
//   it, the active option is announced through aria-activedescendant);
// - searchable: the trigger is a button that opens the panel, and the search box in the
//   panel is the combobox (aria-autocomplete=list) controlling the listbox.
// Keyboard: ↓/↑/Enter/Space/Alt+↓ open; ↓/↑ move, Home/End first/last, PageUp/PageDown ±10;
// Enter (and Space when not searchable) choose; typing jumps to a matching option (or
// filters when searchable); Esc closes this list only (never the dialog around it);
// Tab closes without choosing; Backspace/Delete on the closed field clears it (clearable) or
// removes the last chip (multiple).
// Positioning: floating.ts (flip/shift, height limited to the viewport, top layer).
import { IconCheck, IconSelector, IconPlus, IconSearch, IconX } from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, ref, useAttrs, useId, useSlots, shallowRef, watch, type StyleValue } from 'vue'
import BaseAvatar from './BaseAvatar.vue'
import BaseBadge from './BaseBadge.vue'
import Spinner from './Spinner.vue'
import { useFieldControl } from './field'
import { supportsPopover, useFloating } from './floating'
import type { SelectOption } from './types'

type Model = Multi extends true ? V[] : V | '' | null | undefined

defineOptions({ inheritAttrs: false })

const props = withDefaults(
  defineProps<{
    options?: SelectOption<V>[]
    multiple?: Multi
    /** Show a search box in the panel. Default: when there are more than 8 options or a loader. */
    searchable?: boolean
    /** Show a clear (×) button when there is a value. */
    clearable?: boolean
    placeholder?: string
    id?: string
    invalid?: boolean
    disabled?: boolean
    size?: 'sm' | 'md'
    /** Accessible name of the list when the control is not inside a FormField. */
    label?: string
    searchPlaceholder?: string
    emptyText?: string
    /** Shows a spinner in place of the list (options still being fetched by the caller). */
    loading?: boolean
    /** Async options: called (debounced) with the search text; aborted when superseded. */
    loader?: (query: string, signal: AbortSignal) => Promise<SelectOption<V>[]>
    /** Minimum search length before `loader` runs. */
    minQuery?: number
    /** Shown while the search text is shorter than `minQuery`. */
    idleText?: string
    /** Offer the search text as a new option ("Add …"); return null to refuse it. */
    create?: (query: string) => SelectOption<V> | null
    placement?: 'bottom-start' | 'bottom-end'
    panelClass?: string
    /** Chips shown before collapsing the rest into "+N" (multiple). */
    maxChips?: number
    /** Close after choosing. Default: single → true, multiple → false. */
    closeOnSelect?: boolean
  }>(),
  {
    options: () => [],
    searchable: undefined,
    clearable: false,
    invalid: false,
    disabled: false,
    size: 'md',
    searchPlaceholder: 'Search',
    emptyText: 'No matches',
    loading: false,
    minQuery: 0,
    idleText: 'Type to search',
    placement: 'bottom-start',
    maxChips: 3,
    closeOnSelect: undefined,
  },
)
const model = defineModel<Model>({ required: true })
const emit = defineEmits<{ select: [option: SelectOption<V>]; open: []; close: [] }>()
defineSlots<{
  option?(p: { option: SelectOption<V>; selected: boolean; active: boolean }): unknown
  value?(p: { option: SelectOption<V> }): unknown
  chip?(p: { option: SelectOption<V>; remove: () => void }): unknown
  trigger?(p: { attrs: Record<string, unknown>; open: boolean; selected: SelectOption<V>[] }): unknown
  footer?(p: { close: () => void }): unknown
}>()

const attrs = useAttrs()
const field = useFieldControl(props)
const uid = useId()
const listId = `${uid}-list`
const optId = (i: number) => `${uid}-opt-${i}`

const open = ref(false)
const query = ref('')
const active = ref(-1)
const slots = useSlots()
const root = ref<HTMLElement | null>(null)
const control = ref<HTMLElement | null>(null)
const anchor = ref<HTMLElement | null>(null)
// The focusable trigger: the built-in one or whatever element a #trigger slot bound `attrs` to.
const triggerEl = () => root.value?.querySelector<HTMLElement>('[data-select-trigger]') ?? null
const panel = ref<HTMLElement | null>(null)
const search = ref<HTMLInputElement | null>(null)

const isMulti = computed(() => props.multiple === true)
const isSearchable = computed(() => props.searchable ?? (!!props.loader || !!props.create || props.options.length > 8))
const shouldClose = computed(() => props.closeOnSelect ?? !isMulti.value)

// ---- values ----
const values = computed<V[]>(() => {
  const m = model.value as unknown
  if (Array.isArray(m)) return m as V[]
  return m === '' || m === null || m === undefined ? [] : [m as V]
})
const isSelected = (v: V) => values.value.includes(v)

// Every option seen (static, loaded, created), so chips and labels survive a new search.
const known = new Map<V, SelectOption<V>>()
const knownTick = ref(0)
function remember(list: SelectOption<V>[]) {
  for (const o of list) known.set(o.value, o)
  knownTick.value++
}
watch(() => props.options, remember, { immediate: true })

const selectedOptions = computed<SelectOption<V>[]>(() => {
  void knownTick.value
  return values.value.map((v) => known.get(v) ?? { value: v, label: String(v) })
})

// ---- async loading ----
const loaded = shallowRef<SelectOption<V>[]>([])
const loadingAsync = ref(false)
const loadError = ref(false)
let ctrl: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | undefined
function runLoader(q: string, delay: number) {
  if (!props.loader) return
  clearTimeout(timer)
  ctrl?.abort()
  if (q.trim().length < props.minQuery) {
    loaded.value = []
    loadingAsync.value = false
    return
  }
  loadingAsync.value = true
  timer = setTimeout(async () => {
    const own = new AbortController()
    ctrl = own
    try {
      const res = await props.loader!(q.trim(), own.signal)
      if (own.signal.aborted) return
      remember(res)
      loaded.value = res
      loadError.value = false
    } catch (err) {
      if (own.signal.aborted || (err instanceof DOMException && err.name === 'AbortError')) return
      loadError.value = true
      loaded.value = []
    } finally {
      if (ctrl === own) loadingAsync.value = false
    }
  }, delay)
}
onBeforeUnmount(() => {
  clearTimeout(timer)
  ctrl?.abort()
})

// ---- filtering, groups, flat order ----
const fold = (s: string) => s.normalize('NFD').replace(/\p{M}/gu, '').toLowerCase()
const source = computed(() => (props.loader ? loaded.value : props.options))
const filtered = computed(() => {
  const q = fold(query.value.trim())
  if (!q || props.loader) return source.value
  return source.value.filter((o) => fold([o.label, o.hint ?? '', o.keywords ?? '', o.group ?? '', String(o.value)].join(' ')).includes(q))
})
const creatable = computed<SelectOption<V> | null>(() => {
  const q = query.value.trim()
  if (!props.create || !q) return null
  const opt = props.create(q)
  if (!opt) return null
  const exists = [...filtered.value, ...selectedOptions.value].some((o) => o.value === opt.value || fold(o.label) === fold(q))
  return exists ? null : opt
})

interface Row {
  option: SelectOption<V>
  index: number
  created?: boolean
}
const sections = computed(() => {
  const order: (string | undefined)[] = []
  const byGroup = new Map<string | undefined, SelectOption<V>[]>()
  for (const o of filtered.value) {
    if (!byGroup.has(o.group)) {
      byGroup.set(o.group, [])
      order.push(o.group)
    }
    byGroup.get(o.group)!.push(o)
  }
  let i = 0
  const out: { group: string | undefined; rows: Row[] }[] = order.map((g) => ({ group: g, rows: byGroup.get(g)!.map((option) => ({ option, index: i++ })) }))
  if (creatable.value) out.push({ group: undefined, rows: [{ option: creatable.value, index: i++, created: true }] })
  return out
})
const flat = computed(() => sections.value.flatMap((s) => s.rows))
const enabled = (i: number) => !!flat.value[i] && !flat.value[i]!.option.disabled

function move(from: number, step: number) {
  const n = flat.value.length
  if (!n) return -1
  let i = from
  for (let k = 0; k < n; k++) {
    i = step > 0 ? Math.min(n - 1, i + step) : Math.max(0, i + step)
    if (enabled(i)) return i
    if ((step > 0 && i === n - 1) || (step < 0 && i === 0)) break
  }
  // Nothing enabled further on: stay put (or settle on the nearest enabled option).
  return enabled(from) ? from : flat.value.findIndex((r) => !r.option.disabled)
}
const first = () => move(-1, 1)
const last = () => move(flat.value.length, -1)

watch(active, async (i) => {
  if (i < 0 || !open.value) return
  await nextTick()
  document.getElementById(optId(i))?.scrollIntoView({ block: 'nearest' })
})
watch(flat, () => {
  if (!open.value) return
  if (!enabled(active.value)) active.value = query.value ? first() : initialActive()
})
watch(query, (q) => {
  if (!open.value) return
  if (props.loader) runLoader(q, 200)
  else active.value = first()
})

function initialActive() {
  const sel = flat.value.findIndex((r) => isSelected(r.option.value) && !r.option.disabled)
  return sel >= 0 ? sel : first()
}

// ---- open / close ----
const { style: panelStyle } = useFloating(anchor, panel, open, {
  placement: () => props.placement,
  matchWidth: () => 'min',
  maxHeight: 360,
})

async function show(at: 'selected' | 'first' | 'last' = 'selected', seed = '') {
  if (props.disabled || open.value) return
  query.value = seed
  anchor.value = slots.trigger ? triggerEl() : control.value
  open.value = true
  emit('open')
  if (props.loader) runLoader(seed, seed ? 200 : 0)
  await nextTick()
  active.value = at === 'first' ? first() : at === 'last' ? last() : initialActive()
  if (isSearchable.value) search.value?.focus({ preventScroll: true })
}
function close(returnFocus = true) {
  if (!open.value) return
  open.value = false
  query.value = ''
  active.value = -1
  emit('close')
  if (returnFocus) triggerEl()?.focus({ preventScroll: true })
}
function toggle() {
  if (open.value) close()
  else void show()
}

function onOutside(e: PointerEvent) {
  const t = e.target as Node | null
  if (!t || !t.isConnected) return
  if (root.value && !root.value.contains(t)) close(false)
}
watch(open, (v) => {
  if (v) document.addEventListener('pointerdown', onOutside, true)
  else document.removeEventListener('pointerdown', onOutside, true)
})
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOutside, true))

function onFocusOut(e: FocusEvent) {
  const next = e.relatedTarget as Node | null
  if (open.value && next && root.value && !root.value.contains(next)) close(false)
}

// ---- choosing ----
function choose(row: Row | undefined) {
  if (!row || row.option.disabled) return
  const o = row.option
  if (row.created) remember([o])
  if (isMulti.value) {
    const next = isSelected(o.value) ? values.value.filter((v) => v !== o.value) : [...values.value, o.value]
    model.value = next as unknown as Model
  } else {
    model.value = o.value as unknown as Model
  }
  emit('select', o)
  if (row.created) query.value = ''
  if (shouldClose.value) close()
}
function remove(v: V) {
  model.value = values.value.filter((x) => x !== v) as unknown as Model
}
function clear() {
  model.value = (isMulti.value ? [] : '') as unknown as Model
  if (!open.value) triggerEl()?.focus({ preventScroll: true })
}
const hasValue = computed(() => values.value.length > 0)

// ---- keyboard ----
let typed = ''
let typedAt = 0
let keyToggleAt = 0
function typeAhead(ch: string) {
  const now = Date.now()
  typed = now - typedAt > 600 ? ch : typed + ch
  typedAt = now
  const rows = flat.value
  if (!rows.length) return
  const same = typed.length > 1 && [...typed].every((c) => c === typed[0])
  const needle = fold(same ? typed[0]! : typed)
  const start = same || typed.length === 1 ? active.value + 1 : Math.max(0, active.value)
  for (let k = 0; k < rows.length; k++) {
    const i = (start + k) % rows.length
    if (enabled(i) && fold(rows[i]!.option.label).startsWith(needle)) {
      active.value = i
      return
    }
  }
}

function onKeydown(e: KeyboardEvent) {
  if (props.disabled) return
  const fromSearch = e.target === search.value
  const isButton = (e.currentTarget as HTMLElement | null)?.tagName === 'BUTTON'
  const printable = e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey

  if (!open.value) {
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp':
        e.preventDefault()
        void show(e.key === 'ArrowUp' && !hasValue.value ? 'last' : 'selected')
        return
      case 'Home':
      case 'End':
        e.preventDefault()
        void show(e.key === 'Home' ? 'first' : 'last')
        return
      case 'Enter':
      case ' ':
        if (isButton) return // a <button> trigger opens through its own click
        e.preventDefault()
        keyToggleAt = Date.now()
        void show()
        return
      case 'Backspace':
      case 'Delete':
        if (isMulti.value && values.value.length) {
          e.preventDefault()
          remove(values.value[values.value.length - 1]!)
        } else if (props.clearable && hasValue.value) {
          e.preventDefault()
          clear()
        }
        return
      default:
        if (!printable) return
        e.preventDefault()
        if (isSearchable.value) void show('first', e.key)
        else void show().then(() => typeAhead(e.key))
        return
    }
  }

  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault()
      active.value = e.altKey ? active.value : move(active.value, 1)
      return
    case 'ArrowUp':
      e.preventDefault()
      if (e.altKey) {
        choose(flat.value[active.value])
        if (open.value) close()
      } else active.value = move(active.value, -1)
      return
    case 'PageDown':
      e.preventDefault()
      active.value = move(active.value, 10)
      return
    case 'PageUp':
      e.preventDefault()
      active.value = move(active.value, -10)
      return
    case 'Home':
      e.preventDefault()
      active.value = first()
      return
    case 'End':
      e.preventDefault()
      active.value = last()
      return
    case 'Enter':
      e.preventDefault()
      keyToggleAt = Date.now()
      choose(flat.value[active.value])
      return
    case 'Escape':
      // This list only: preventDefault also stops a native <dialog> from closing.
      e.preventDefault()
      e.stopPropagation()
      close()
      return
    case 'Tab':
      if (fromSearch && e.shiftKey) {
        e.preventDefault()
        close()
      } else if (!fromSearch) close(false)
      return
    case ' ':
      if (fromSearch) return
      e.preventDefault()
      keyToggleAt = Date.now()
      choose(flat.value[active.value])
      return
    default:
      if (printable && !fromSearch) {
        e.preventDefault()
        typeAhead(e.key)
      }
  }
}

function onTriggerClick(e: MouseEvent) {
  // A keyboard-synthesised click right after a key already handled (custom <button> triggers).
  if (e.detail === 0 && Date.now() - keyToggleAt < 400) return
  toggle()
}
function onControlClick(e: MouseEvent) {
  if (props.disabled) return
  const t = e.target as HTMLElement
  if (t.closest('button') && !t.closest('[data-select-trigger]')) return // chip × / clear
  if (t.closest('[data-select-trigger]')) return // handled by the trigger itself
  toggle()
  triggerEl()?.focus({ preventScroll: true })
}

// ---- ARIA wiring ----
const ariaLabel = computed(() => (attrs['aria-label'] as string | undefined) ?? undefined)
const labelledBy = computed(() => (ariaLabel.value ? undefined : field.labelId.value))
const listName = computed(() => props.label ?? ariaLabel.value)
const activeId = computed(() => (open.value && active.value >= 0 && flat.value[active.value] ? optId(active.value) : undefined))

const triggerAttrs = computed<Record<string, unknown>>(() => {
  const base: Record<string, unknown> = {
    id: field.id.value,
    'data-select-trigger': '',
    'aria-haspopup': 'listbox',
    'aria-expanded': open.value ? 'true' : 'false',
    'aria-controls': open.value ? listId : undefined,
    'aria-describedby': field.describedBy.value,
    'aria-invalid': field.invalid.value ? 'true' : undefined,
    'aria-disabled': props.disabled ? 'true' : undefined,
    'aria-label': ariaLabel.value,
    'aria-labelledby': labelledBy.value,
    onKeydown,
    onClick: onTriggerClick,
  }
  // aria-required is allowed on a combobox, not on the button that opens a searchable list.
  if (isSearchable.value) base.role = 'button'
  else {
    base.role = 'combobox'
    base['aria-activedescendant'] = activeId.value
    base['aria-required'] = field.required.value ? 'true' : undefined
  }
  return base
})
const passthrough = computed(() => {
  const { class: cls, style, 'aria-label': _al, ...rest } = attrs
  void _al
  return { cls, style: style as StyleValue, rest }
})

const selectedSummary = computed(() => selectedOptions.value.map((o) => o.label).join(', '))
const chips = computed(() => selectedOptions.value.slice(0, props.maxChips))
const overflow = computed(() => Math.max(0, selectedOptions.value.length - props.maxChips))
const showList = computed(() => !(props.loading || (props.loader && loadingAsync.value && !loaded.value.length)))
const statusText = computed(() => {
  if (props.loading || (props.loader && loadingAsync.value && !flat.value.length)) return 'Loading…'
  if (loadError.value) return 'Could not load options. Try again.'
  if (props.loader && query.value.trim().length < props.minQuery) return props.idleText
  if (!flat.value.length) return props.emptyText
  return ''
})

defineExpose({ focus: () => triggerEl()?.focus(), open: () => show(), close })
</script>

<template>
  <div ref="root" class="relative min-w-0" :class="passthrough.cls" :style="passthrough.style" @focusout="onFocusOut">
    <!-- Custom trigger (project switcher, filter chips): bind `attrs` onto a focusable element. -->
    <slot v-if="$slots.trigger" name="trigger" :attrs="{ ...triggerAttrs, ...passthrough.rest }" :open="open" :selected="selectedOptions" />

    <!-- Default field-like control. -->
    <div
      v-else
      ref="control"
      class="lg-input flex items-center gap-1.5 !pr-1.5"
      :class="[
        size === 'sm' ? 'min-h-[34px]' : 'min-h-9',
        isMulti && hasValue ? '!h-auto flex-wrap !pl-1.5 py-1' : size === 'sm' ? 'h-[34px]' : '',
        disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer',
        open ? '!border-text' : '',
        field.invalid.value ? '!border-danger' : '',
        'has-[[data-select-trigger]:focus-visible]:outline-2 has-[[data-select-trigger]:focus-visible]:outline-offset-2 has-[[data-select-trigger]:focus-visible]:outline-ring has-[[data-select-trigger]:focus-visible]:outline-solid',
      ]"
      @click="onControlClick"
    >
      <template v-if="isMulti">
        <template v-for="o in chips" :key="String(o.value)">
          <slot name="chip" :option="o" :remove="() => remove(o.value)">
            <BaseBadge removable :remove-label="`Remove ${o.label}`" :remove-disabled="disabled" class="max-w-full !text-text" @remove="remove(o.value)">
              <BaseAvatar v-if="o.avatar" :name="o.label" :role="o.avatar" size="2xs" decorative />
              <span class="lg-ell">{{ o.label }}</span>
            </BaseBadge>
          </slot>
        </template>
        <span v-if="overflow" class="lg-tag" v-tooltip="selectedSummary">+{{ overflow }}</span>
      </template>
      <div
        v-bind="{ ...triggerAttrs, ...passthrough.rest }"
        :tabindex="disabled ? -1 : 0"
        class="flex min-w-[2ch] flex-1 items-center gap-2 self-stretch outline-none focus-visible:outline-none"
        :class="isMulti && hasValue ? 'min-h-6' : ''"
      >
        <template v-if="isMulti">
          <span v-if="!hasValue" class="lg-ell text-muted">{{ placeholder ?? 'Select…' }}</span>
          <span v-else class="sr-only">{{ selectedSummary }}</span>
        </template>
        <template v-else-if="selectedOptions[0]">
          <slot name="value" :option="selectedOptions[0]">
            <BaseAvatar v-if="selectedOptions[0].avatar" :name="selectedOptions[0].label" :role="selectedOptions[0].avatar" size="xs" decorative />
            <span class="lg-ell">{{ selectedOptions[0].label }}</span>
          </slot>
        </template>
        <span v-else class="lg-ell text-muted">{{ placeholder ?? 'Select…' }}</span>
      </div>
      <button
        v-if="clearable && hasValue && !disabled"
        type="button"
        tabindex="-1"
        class="lg-hov inline-flex size-6 flex-none items-center justify-center rounded border-0 bg-transparent p-0 text-muted"
        aria-label="Clear selection"
        v-tooltip="'Clear selection'"
        @click.stop="clear"
      >
        <IconX :size="14" stroke="1.75" aria-hidden="true" />
      </button>
      <IconSelector :size="16" stroke="1.75" aria-hidden="true" class="flex-none text-muted" />
    </div>

    <div
      v-if="open"
      ref="panel"
      class="lg-float flex flex-col !overflow-hidden"
      :class="panelClass"
      :popover="supportsPopover ? 'manual' : undefined"
      :style="panelStyle"
    >
      <div v-if="isSearchable" class="relative flex-none border-b border-line p-1.5">
        <IconSearch :size="14" stroke="1.75" aria-hidden="true" class="pointer-events-none absolute top-[15px] left-3.5 text-muted" />
        <input
          ref="search"
          v-model="query"
          type="text"
          role="combobox"
          autocomplete="off"
          spellcheck="false"
          :aria-label="listName ? `Search ${listName}` : 'Search options'"
          aria-autocomplete="list"
          aria-expanded="true"
          :aria-controls="listId"
          :aria-activedescendant="activeId"
          :placeholder="searchPlaceholder"
          class="h-8 w-full rounded border border-border bg-bg pr-7 pl-7 text-[13px] outline-none focus-visible:border-border-strong focus-visible:outline-none"
          @keydown="onKeydown"
        />
        <Spinner v-if="loadingAsync && flat.length" :size="12" class="absolute top-[16px] right-3.5" />
      </div>
      <div
        :id="listId"
        role="listbox"
        :aria-label="listName"
        :aria-labelledby="listName ? undefined : field.labelId.value"
        :aria-multiselectable="isMulti ? 'true' : undefined"
        :aria-busy="loading || loadingAsync ? 'true' : undefined"
        class="min-h-0 flex-1 overflow-auto p-1"
        tabindex="-1"
        @mousedown.prevent
      >
        <template v-if="showList">
          <div
            v-for="(s, si) in sections"
            :key="s.group ?? `_${si}`"
            :role="s.group ? 'group' : 'presentation'"
            :aria-labelledby="s.group ? `${uid}-g-${si}` : undefined"
          >
            <div v-if="s.group" :id="`${uid}-g-${si}`" role="presentation" class="lg-section-label !px-2 !pt-2 !pb-1">{{ s.group }}</div>
            <div
              v-for="r in s.rows"
              :id="optId(r.index)"
              :key="r.created ? '__create' : String(r.option.value)"
              role="option"
              class="lg-option"
              :class="r.created ? 'border-t border-line' : ''"
              :data-active="r.index === active ? 'true' : undefined"
              :aria-selected="isSelected(r.option.value) ? 'true' : 'false'"
              :aria-disabled="r.option.disabled ? 'true' : undefined"
              @mousemove="!r.option.disabled && active !== r.index && (active = r.index)"
              @click="choose(r)"
            >
              <template v-if="r.created">
                <IconPlus :size="14" stroke="1.75" aria-hidden="true" class="flex-none text-muted" />
                <span class="lg-ell flex-1">Add “{{ r.option.label }}”</span>
              </template>
              <template v-else>
                <span
                  v-if="isMulti"
                  aria-hidden="true"
                  class="inline-flex size-4 flex-none items-center justify-center rounded-[3px] border"
                  :class="isSelected(r.option.value) ? 'border-primary bg-primary text-primary-fg' : 'border-border-strong bg-surface'"
                >
                  <IconCheck v-if="isSelected(r.option.value)" :size="12" stroke="2.5" />
                </span>
                <slot name="option" :option="r.option" :selected="isSelected(r.option.value)" :active="r.index === active">
                  <BaseAvatar v-if="r.option.avatar" :name="r.option.label" :role="r.option.avatar" size="xs" decorative />
                  <span class="lg-ell flex-1">{{ r.option.label }}</span>
                  <span v-if="r.option.hint" class="lg-ell max-w-[50%] text-xs font-normal text-muted">{{ r.option.hint }}</span>
                </slot>
                <IconCheck v-if="!isMulti && isSelected(r.option.value)" :size="14" stroke="2" aria-hidden="true" class="ml-auto flex-none" />
              </template>
            </div>
          </div>
        </template>
        <div v-if="statusText" role="presentation" class="flex items-center gap-2 px-2 py-2 text-muted" :class="loadError ? '!text-danger' : ''">
          <Spinner v-if="statusText === 'Loading…'" :size="14" />
          {{ statusText }}
        </div>
      </div>
      <div v-if="(isMulti && clearable && hasValue) || $slots.footer" class="flex-none border-t border-line p-1">
        <button v-if="isMulti && clearable && hasValue" type="button" class="lg-nav lg-hov w-full border-0 bg-transparent text-left text-muted" @mousedown.prevent @click="clear">
          Clear selection ({{ values.length }})
        </button>
        <slot name="footer" :close="() => close()" />
      </div>
      <span class="sr-only" aria-live="polite">{{ statusText && statusText !== 'Loading…' ? statusText : '' }}</span>
    </div>
  </div>
</template>
