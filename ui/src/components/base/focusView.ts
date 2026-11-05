// Focus view: shows an existing element as a near-full-viewport panel without moving or
// re-rendering it (the editor inside keeps its state, history and cursor). Where supported the
// element is promoted to the top layer with `popover="manual"` (above a native modal <dialog>
// and any transformed ancestor); otherwise it is simply `position: fixed`. Styling:
// `.lg-focus-view` in style.css.
//
// Keyboard (listened on the element itself): Ctrl/Cmd+Shift+E toggles; Esc collapses when no
// inner list/menu/dialog handled it first (those call preventDefault / stopPropagation).
import { onBeforeUnmount, watch, type Ref } from 'vue'
import { supportsPopover } from './floating'

export const isExpandShortcut = (e: KeyboardEvent) => (e.metaKey || e.ctrlKey) && e.shiftKey && (e.key === 'e' || e.key === 'E')

export function useFocusView(el: Ref<HTMLElement | null>, expanded: Ref<boolean>, enabled: () => boolean = () => true) {
  function apply(node: HTMLElement, on: boolean) {
    if (on) {
      node.classList.add('lg-focus-view')
      if (supportsPopover && !node.hasAttribute('popover')) {
        node.setAttribute('popover', 'manual')
        try {
          node.showPopover()
        } catch {
          // fixed positioning still applies
        }
      }
    } else {
      if (supportsPopover && node.hasAttribute('popover')) {
        try {
          if (node.matches(':popover-open')) node.hidePopover()
        } catch {
          // ignore
        }
        node.removeAttribute('popover')
      }
      node.classList.remove('lg-focus-view')
    }
  }

  function onKey(e: KeyboardEvent) {
    if (!enabled()) return
    if (isExpandShortcut(e)) {
      e.preventDefault()
      expanded.value = !expanded.value
      return
    }
    // ProseMirror marks every Esc typed in the text as handled without doing anything with it,
    // so that one still collapses (otherwise Esc never worked while the cursor was in the editor).
    const fromText = !!(e.target as Element | null)?.closest?.('.ProseMirror')
    if (e.key !== 'Escape' || !expanded.value || (e.defaultPrevented && !fromText)) return
    // A dialog opened from inside (e.g. "Insert link") handles its own Esc.
    const dlg = (e.target as Element | null)?.closest?.('dialog')
    if (dlg && el.value?.contains(dlg)) return
    e.preventDefault() // also keeps a surrounding modal <dialog> open
    e.stopPropagation()
    expanded.value = false
  }

  let bound: HTMLElement | null = null
  watch(
    [el, expanded],
    ([node, on], prev) => {
      const prevNode = prev?.[0]
      if (prevNode && prevNode !== node) apply(prevNode, false)
      if (bound !== node) {
        bound?.removeEventListener('keydown', onKey)
        node?.addEventListener('keydown', onKey)
        bound = node
      }
      if (node) apply(node, on && enabled())
    },
    { flush: 'post', immediate: true },
  )
  onBeforeUnmount(() => {
    bound?.removeEventListener('keydown', onKey)
    if (el.value) apply(el.value, false)
  })
}
