<script setup lang="ts" generic="T">
// View-first field: the value reads as normal text with a dotted underline and a small pencil
// at the end; the editor (any base input/select passed in the #editor slot) appears only after
// the pencil or the value is activated. Only one inline field is in edit mode at a time.
//
// Modes:
// - select: choosing a value commits at once; closing the list without choosing cancels.
//   Wire the slot's `commit` to @update:model-value and `closed` to @close.
// - text: Enter or leaving the field commits, Esc cancels; Save/Cancel icon buttons.
// - multi: the draft collects choices (tags); Save / leaving the field commits, Esc cancels.
// - dialog: the pencil emits `open` (the caller shows its own dialog); no inline editor.
//
// `@save` receives the value and may return a promise; a rejection stays in edit mode and shows
// the problem text inline. Esc and a finished save return focus to the pencil.
import { IconCheck, IconPencil, IconX } from '@tabler/icons-vue'
import { computed, nextTick, onBeforeUnmount, ref, useId, watch, type Ref } from 'vue'
import { ApiError, errorMessage } from '@/api/errors'
import IconButton from './IconButton.vue'
import Spinner from './Spinner.vue'
import { activeInlineEdit } from './inlineEdit'

const props = withDefaults(
  defineProps<{
    /** Field name, used in "Edit <label>". */
    label: string
    value?: T
    /** Text shown in display mode when there is no default slot. */
    display?: string
    /** Shown (muted) when the value is empty and there is no default slot. */
    emptyText?: string
    editable?: boolean
    /** Temporarily not editable (another change is saving). */
    disabled?: boolean
    mode?: 'select' | 'text' | 'multi' | 'dialog'
    /** Commits the value; a rejection keeps the editor open with the error. */
    onSave?: (value: T) => unknown
  }>(),
  { editable: true, disabled: false, mode: 'select', emptyText: '—', display: undefined, value: undefined, onSave: undefined },
)
const emit = defineEmits<{ open: []; editing: [on: boolean] }>()
defineSlots<{
  default?(): unknown
  editor?(p: {
    editorRef: (el: unknown) => void
    draft: T
    setDraft: (v: unknown) => void
    /** Commits `v` (or the draft). Typed loosely so selects can bind their update event. */
    commit: (v?: unknown) => void
    cancel: () => void
    closed: () => void
    saving: boolean
    invalid: boolean
    errorId: string
  }): unknown
}>()

const uid = useId()
const token = Symbol(uid)
const errorId = `${uid}-err`
const root = ref<HTMLElement | null>(null)
const pencil = ref<HTMLButtonElement | null>(null)
const box = ref<HTMLElement | null>(null)
let editor: { open?: () => void; focus?: () => void } | null = null
const editorRef = (el: unknown) => {
  editor = (el as typeof editor) ?? null
}

const editing = ref(false)
const saving = ref(false)
const error = ref<unknown>(null)
const draft = ref(props.value) as Ref<T>
const canEdit = computed(() => props.editable)
let failed: unknown = undefined

const empty = (v: unknown) => v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0)
const shown = computed(() => props.display ?? (empty(props.value) ? '' : String(props.value)))
function same(a: unknown, b: unknown) {
  if (empty(a) && empty(b)) return true
  if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((x, i) => x === b[i])
  return a === b
}
const errorText = computed(() => {
  const e = error.value
  if (e === null || e === undefined) return ''
  if (typeof e === 'string') return e
  if (e instanceof ApiError && e.fieldErrors.length) return e.fieldErrors.map((f) => f.message).join(' ')
  return errorMessage(e)
})

async function start() {
  if (!canEdit.value || props.disabled) return
  if (props.mode === 'dialog') {
    emit('open')
    return
  }
  activeInlineEdit.value = token
  draft.value = (Array.isArray(props.value) ? [...props.value] : props.value) as T
  error.value = null
  editing.value = true
  emit('editing', true)
  await nextTick()
  if (editor?.focus) editor.focus()
  else box.value?.querySelector<HTMLElement>('input, textarea, select, button, [tabindex]')?.focus()
  if (props.mode !== 'text') editor?.open?.()
}

function finish() {
  const wasEditing = editing.value
  editing.value = false
  error.value = null
  if (activeInlineEdit.value === token) activeInlineEdit.value = null
  if (!wasEditing) return
  emit('editing', false)
  // Return focus to the pencil unless the person has already moved on to something else.
  const a = document.activeElement
  const keep = !a || a === document.body || (root.value?.contains(a) ?? false)
  if (keep) void nextTick(() => pencil.value?.focus({ preventScroll: true }))
}

function cancel() {
  if (saving.value) return
  finish()
}

async function commit(v?: unknown) {
  if (!editing.value || saving.value) return
  if (v !== undefined) draft.value = v as T
  const next = draft.value
  if (same(next, props.value) || !props.onSave) {
    finish()
    return
  }
  saving.value = true
  error.value = null
  try {
    await props.onSave(next)
    saving.value = false
    finish()
  } catch (err) {
    saving.value = false
    error.value = err
    // A select shows the stored value again; typed text is kept so it can be corrected.
    if (props.mode === 'select') draft.value = props.value as T
  }
}

/** The select list closed: after a choice the commit is running; otherwise it was dismissed. */
function closed() {
  if (props.mode === 'select' && editing.value && !saving.value && !error.value) cancel()
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape' && !e.defaultPrevented) {
    e.preventDefault()
    e.stopPropagation()
    cancel()
  } else if (e.key === 'Enter' && props.mode === 'text' && (e.target as HTMLElement).tagName === 'INPUT') {
    e.preventDefault()
    void commit()
  }
}
function onFocusOut(e: FocusEvent) {
  if (props.mode === 'select' || !editing.value || saving.value) return
  const next = e.relatedTarget as Node | null
  if (next && box.value?.contains(next)) return
  // Leaving the field commits a change (unless that exact value just failed).
  if (error.value && same(draft.value, failed)) return
  void commit()
}
watch(error, (e) => {
  failed = e ? draft.value : undefined
})

// Another inline field started editing: this one steps back without saving.
watch(activeInlineEdit, (t) => {
  if (editing.value && t !== token && !saving.value) {
    editing.value = false
    error.value = null
    emit('editing', false)
  }
})
// A refreshed value replaces an untouched select draft; typed text is kept.
watch(
  () => props.value,
  (v) => {
    if (editing.value && !saving.value && props.mode === 'select') draft.value = v as T
  },
)
// Losing permission (archived, role change) ends editing.
watch(
  () => props.editable,
  (on) => {
    if (!on && editing.value) finish()
  },
)
onBeforeUnmount(() => {
  if (activeInlineEdit.value === token) activeInlineEdit.value = null
})

const setDraft = (v: unknown) => {
  draft.value = v as T
}
defineExpose({ start, cancel })
</script>

<template>
  <div ref="root" class="flex min-w-0 flex-col gap-1">
    <div v-if="!editing" class="flex min-w-0 items-center gap-1">
      <span
        v-if="canEdit"
        class="lg-inline-value min-w-0"
        :class="{ 'opacity-60': disabled }"
        @click="start"
      >
        <slot>
          <span v-if="shown" class="break-words">{{ shown }}</span>
          <span v-else class="text-muted">{{ emptyText }}</span>
        </slot>
      </span>
      <span v-else class="min-w-0">
        <slot>
          <span v-if="shown" class="break-words">{{ shown }}</span>
          <span v-else class="text-muted">{{ emptyText }}</span>
        </slot>
      </span>
      <button
        v-if="canEdit"
        ref="pencil"
        type="button"
        class="lg-inline-pencil"
        :aria-label="`Edit ${label}`"
        v-tooltip="`Edit ${label}`"
        :aria-haspopup="mode === 'dialog' ? 'dialog' : undefined"
        :disabled="disabled"
        @click="start"
      >
        <IconPencil :size="14" stroke="1.75" aria-hidden="true" />
      </button>
    </div>
    <div v-else ref="box" class="flex min-w-0 items-center gap-1" @keydown="onKeydown" @focusout="onFocusOut">
      <div class="min-w-0 flex-1">
        <slot
          name="editor"
          :editor-ref="editorRef"
          :draft="draft"
          :set-draft="setDraft"
          :commit="commit"
          :cancel="cancel"
          :closed="closed"
          :saving="saving"
          :invalid="!!errorText"
          :error-id="errorId"
        />
      </div>
      <Spinner v-if="saving" :size="14" label="Saving" class="flex-none text-muted" />
      <template v-if="mode !== 'select'">
        <IconButton label="Save" :disabled="saving" class="!h-7 !w-7" @mousedown.prevent @click="commit()">
          <IconCheck :size="14" stroke="1.75" />
        </IconButton>
        <IconButton label="Cancel" :disabled="saving" class="!h-7 !w-7" @mousedown.prevent @click="cancel">
          <IconX :size="14" stroke="1.75" />
        </IconButton>
      </template>
    </div>
    <p v-if="errorText && editing" :id="errorId" role="alert" class="m-0 text-xs text-danger">{{ errorText }}</p>
  </div>
</template>
