<script setup lang="ts">
// WYSIWYG Markdown editor (Tiptap). Formatting renders in place as you type, Markdown input
// rules apply (**bold**, `code`, "- " lists, "> " quotes), and v-model is a Markdown string.
// The schema only knows the nodes/marks below, so pasted HTML is reduced to them.
import {
  IconArrowsMaximize,
  IconArrowsMinimize,
  IconBlockquote,
  IconBold,
  IconCode,
  IconItalic,
  IconLink,
  IconList,
  IconListNumbers,
  IconSourceCode,
} from '@tabler/icons-vue'
import { Markdown } from '@tiptap/markdown'
import { Placeholder } from '@tiptap/extensions'
import StarterKit from '@tiptap/starter-kit'
import { EditorContent, useEditor } from '@tiptap/vue-3'
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import BaseButton from './BaseButton.vue'
import BaseModal from './BaseModal.vue'
import FormField from './FormField.vue'
import IconButton from './IconButton.vue'
import TextInput from './TextInput.vue'
import { useFieldControl } from './field'
import { isExpandShortcut, useFocusView } from './focusView'

const props = withDefaults(
  defineProps<{
    /** Accessible name of the editing area. */
    label: string
    placeholder?: string
    disabled?: boolean
    tone?: 'default' | 'note'
    minHeight?: number
    maxHeight?: number
    id?: string
    invalid?: boolean
    /** Extra aria-describedby ids for the editing area. */
    describedby?: string
    /** Shows an expand/collapse button (Ctrl/Cmd+Shift+E) for a large focus view. */
    expandable?: boolean
    /**
     * The parent expands its own container (v-model:expanded), e.g. a composer that keeps its
     * audience and Send controls in the focus view. Otherwise the editor expands itself.
     */
    expandContainer?: boolean
  }>(),
  { placeholder: '', disabled: false, tone: 'default', minHeight: 76, maxHeight: 360, expandable: false, expandContainer: false },
)
const model = defineModel<string>({ default: '' })
const expanded = defineModel<boolean>('expanded', { default: false })
const emit = defineEmits<{ submit: [] }>()
const { id, describedBy, invalid } = useFieldControl(props)

let lastEmitted = model.value
const tick = ref(0)
const linkOpen = ref(false)
const linkUrl = ref('')
const linkError = ref<string | null>(null)

const SAFE_URL = /^(https?:\/\/|mailto:)/i
const ariaDescribedBy = () => [describedBy.value, props.describedby].filter(Boolean).join(' ')

const editor = useEditor({
  content: model.value,
  contentType: 'markdown',
  editable: !props.disabled,
  extensions: [
    StarterKit.configure({
      heading: false,
      horizontalRule: false,
      underline: false,
      strike: false,
      link: {
        openOnClick: false,
        autolink: true,
        linkOnPaste: true,
        defaultProtocol: 'https',
        // http, https and mailto are built into linkify; registering them again made every
        // further editor (a dialog next to the composer) log "linkifyjs: already initialized".
        isAllowedUri: (url, ctx) => SAFE_URL.test(url) && ctx.defaultValidate(url),
        HTMLAttributes: { rel: 'noopener noreferrer nofollow', target: '_blank' },
      },
    }),
    Placeholder.configure({ placeholder: () => props.placeholder }),
    Markdown,
  ],
  editorProps: {
    attributes: () => ({
      id: id.value,
      role: 'textbox',
      'aria-multiline': 'true',
      'aria-label': props.label,
      ...(ariaDescribedBy() ? { 'aria-describedby': ariaDescribedBy() } : {}),
      ...(invalid.value ? { 'aria-invalid': 'true' } : {}),
      class: 'lg-prose outline-none',
    }),
    handleKeyDown: (_view, e) => {
      const mod = e.metaKey || e.ctrlKey
      if (mod && e.key === 'Enter') {
        e.preventDefault()
        emit('submit')
        return true
      }
      // Self-expanding editors handle the shortcut on their wrapper (useFocusView); a
      // container-expanded one leaves it to the container's listener.
      if (props.expandable && isExpandShortcut(e)) return false
      if (mod && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault()
        openLink()
        return true
      }
      return false
    },
  },
  onUpdate: ({ editor: ed }) => {
    lastEmitted = ed.isEmpty ? '' : ed.getMarkdown()
    model.value = lastEmitted
  },
  onTransaction: () => {
    tick.value++
  },
})

watch(model, (v) => {
  const ed = editor.value
  if (!ed || v === lastEmitted) return
  lastEmitted = v
  ed.commands.setContent(v, { contentType: 'markdown', emitUpdate: false })
})
// No update event: setEditable() emits one by default, and onUpdate would write the editor's
// content into the model. When a draft finishes loading, `disabled` and the model change
// together and this watcher can run first, so that wrote the still-empty editor over the
// loaded draft (the composer then deleted the saved draft on the server).
watch(
  () => props.disabled,
  (d) => editor.value?.setEditable(!d, false),
)
onBeforeUnmount(() => editor.value?.destroy())

function isActive(name: string): boolean {
  void tick.value
  return editor.value?.isActive(name) ?? false
}

const tools = computed(() => [
  { label: 'Bold (Ctrl+B)', icon: IconBold, active: isActive('bold'), run: () => editor.value?.chain().focus().toggleBold().run() },
  { label: 'Italic (Ctrl+I)', icon: IconItalic, active: isActive('italic'), run: () => editor.value?.chain().focus().toggleItalic().run() },
  { label: 'Inline code', icon: IconCode, active: isActive('code'), run: () => editor.value?.chain().focus().toggleCode().run() },
  { label: 'Code block', icon: IconSourceCode, active: isActive('codeBlock'), run: () => editor.value?.chain().focus().toggleCodeBlock().run() },
  { label: 'Link (Ctrl+K)', icon: IconLink, active: isActive('link'), run: openLink },
  { label: 'Bulleted list', icon: IconList, active: isActive('bulletList'), run: () => editor.value?.chain().focus().toggleBulletList().run() },
  { label: 'Numbered list', icon: IconListNumbers, active: isActive('orderedList'), run: () => editor.value?.chain().focus().toggleOrderedList().run() },
  { label: 'Quote', icon: IconBlockquote, active: isActive('blockquote'), run: () => editor.value?.chain().focus().toggleBlockquote().run() },
])

function openLink() {
  if (props.disabled) return
  linkUrl.value = (editor.value?.getAttributes('link').href as string | undefined) ?? ''
  linkError.value = null
  linkOpen.value = true
}

function applyLink() {
  const ed = editor.value
  if (!ed) return
  let url = linkUrl.value.trim()
  if (!url) {
    ed.chain().focus().extendMarkRange('link').unsetLink().run()
    linkOpen.value = false
    return
  }
  if (!/^[a-z]+:/i.test(url)) url = `https://${url}`
  if (!SAFE_URL.test(url)) {
    linkError.value = 'Only http, https and mailto links are allowed.'
    return
  }
  const chain = ed.chain().focus().extendMarkRange('link')
  if (ed.state.selection.empty && !ed.isActive('link')) {
    chain.insertContent({ type: 'text', text: url, marks: [{ type: 'link', attrs: { href: url } }] }).run()
  } else {
    chain.setLink({ href: url }).run()
  }
  linkOpen.value = false
}

function focus() {
  editor.value?.commands.focus('end')
}

/**
 * A click on the editor's padding (outside the text) puts the cursor at the end. A click in the
 * text is left to ProseMirror, which has already placed the cursor there: focusing 'end' on every
 * click moved the cursor to the last line, so earlier lines could not be edited with the mouse.
 */
function onAreaClick(e: MouseEvent) {
  const dom = editor.value?.view.dom
  if (dom && e.target instanceof Node && dom.contains(e.target)) return
  focus()
}

// ---- focus view ----
const root = ref<HTMLElement | null>(null)
useFocusView(root, expanded, () => props.expandable && !props.expandContainer)
function toggleExpanded() {
  expanded.value = !expanded.value
}
// After expanding or collapsing, the cursor stays where it was and the editor keeps focus.
watch(expanded, async () => {
  await nextTick()
  editor.value?.commands.focus(undefined, { scrollIntoView: true })
})
defineExpose({ focus, toggleExpanded })
</script>

<template>
  <div
    ref="root"
    class="flex flex-col rounded-md border bg-surface focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-ring"
    :class="[
      invalid ? 'border-danger' : 'border-border-strong',
      tone === 'note' ? '!bg-note-bg' : '',
      disabled ? 'opacity-60' : '',
      expanded && expandable ? 'min-h-0 flex-1' : '',
    ]"
  >
    <div role="toolbar" :aria-label="`${label} formatting`" class="flex flex-wrap items-center gap-0.5 border-b border-line px-1.5 py-1">
      <IconButton
        v-for="t in tools"
        :key="t.label"
        :label="t.label"
        :pressed="t.active"
        :disabled="disabled"
        class="!size-7"
        :class="t.active ? '!bg-subtle !text-text' : ''"
        @mousedown.prevent
        @click="t.run()"
      >
        <component :is="t.icon" :size="16" stroke="1.75" />
      </IconButton>
      <span class="flex-1"></span>
      <slot name="toolbar-end" />
      <IconButton
        v-if="expandable"
        :label="expanded ? 'Collapse editor' : 'Expand editor'"
        :pressed="expanded"
        class="!size-7"
        :tip="`${expanded ? 'Collapse editor' : 'Expand editor'} (Ctrl/Cmd+Shift+E)`"
        @mousedown.prevent
        @click="toggleExpanded"
      >
        <IconArrowsMinimize v-if="expanded" :size="16" stroke="1.75" />
        <IconArrowsMaximize v-else :size="16" stroke="1.75" />
      </IconButton>
    </div>
    <EditorContent
      :editor="editor"
      class="kase-editor cursor-text overflow-auto px-3 py-2.5 text-[13.5px] leading-normal"
      :class="expandable ? (expanded ? 'min-h-0 flex-1 !h-auto' : 'resize-y') : ''"
      :style="expanded && expandable ? { minHeight: `${minHeight}px`, maxHeight: 'none' } : { minHeight: `${minHeight}px`, maxHeight: `${maxHeight}px` }"
      @click="onAreaClick"
    />

    <BaseModal v-model:open="linkOpen" title="Insert link">
      <form class="flex flex-col gap-3" @submit.prevent="applyLink">
        <FormField label="URL" :error="linkError" hint="Leave empty to remove the link.">
          <TextInput v-model="linkUrl" type="url" />
        </FormField>
        <div class="flex justify-end gap-2">
          <BaseButton @click="linkOpen = false">Cancel</BaseButton>
          <BaseButton type="submit" variant="primary">Apply</BaseButton>
        </div>
      </form>
    </BaseModal>
  </div>
</template>

<style scoped>
.kase-editor :deep(.ProseMirror) {
  min-height: inherit;
  white-space: pre-wrap;
}
.kase-editor :deep(.ProseMirror:focus) {
  outline: none;
}
.kase-editor :deep(p.is-editor-empty:first-child::before) {
  content: attr(data-placeholder);
  float: left;
  height: 0;
  pointer-events: none;
  color: var(--kase-muted);
}
</style>
