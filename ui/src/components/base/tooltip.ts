// Styled tooltip, as a directive: `v-tooltip="'text'"` (multi-line text keeps its line breaks).
//
//   v-tooltip="text"            string, or empty/undefined for none (no tooltip)
//   v-tooltip.truncated="text"  only when the element (or an `.lg-ell` inside it) is cut off
//
// One shared card for the whole app: shown ~300ms after the pointer rests on the element or
// after keyboard focus (:focus-visible), hidden at once on leave, blur, click, scroll, resize
// or Esc (listened for on the document while shown). Touch never shows it (taps stay clicks).
// Positioned with floating-ui (flip + shift) and, where supported, in the top layer so it also
// appears above native modal dialogs.
//
// The trigger always has aria-describedby -> a hidden per-element description with the same text
// (merged with any describedby it already had, restored on unmount), so the hint is available to
// assistive tech without hover; skipped when the text equals the element's aria-label.
// A second directive on the same element (component root + parent) updates the first one rather
// than binding a second set of listeners; still, pass text to a component through its prop.
import { computePosition, flip, offset, shift } from '@floating-ui/dom'
import type { Directive } from 'vue'

type Value = string | null | undefined | false
interface State {
  text: string
  truncated: boolean
  cleanup: () => void
  descId: string
  descEl: HTMLElement
  origDescribedBy: string | null
}

const DELAY = 300
const states = new WeakMap<HTMLElement, State>()
let card: HTMLElement | null = null
let descBox: HTMLElement | null = null
let current: HTMLElement | null = null
let timer: ReturnType<typeof setTimeout> | undefined
let seq = 0

function ensureCard(): HTMLElement {
  if (card) return card
  card = document.createElement('div')
  card.id = 'kase-tooltip'
  card.setAttribute('role', 'tooltip')
  card.className = 'kase-tooltip'
  if ('showPopover' in HTMLElement.prototype) card.setAttribute('popover', 'manual')
  else card.style.display = 'none'
  document.body.appendChild(card)
  return card
}

function isCut(el: HTMLElement): boolean {
  const cut = (n: HTMLElement) => n.scrollWidth > n.clientWidth + 1
  if (cut(el)) return true
  return Array.from(el.querySelectorAll<HTMLElement>('.lg-ell')).some(cut)
}

// Only swallows Escape when a tooltip was actually showing.
function onDocKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && current) {
    e.stopPropagation()
    hide()
  }
}

function hide() {
  clearTimeout(timer)
  timer = undefined
  if (current) {
    current = null
    document.removeEventListener('keydown', onDocKey, true)
  }
  if (!card) return
  if (card.hasAttribute('popover')) {
    if (card.matches(':popover-open')) card.hidePopover()
  } else card.style.display = 'none'
}

function show(el: HTMLElement) {
  const st = states.get(el)
  if (!st || !st.text || !el.isConnected || (st.truncated && !isCut(el))) return
  hide()
  const c = ensureCard()
  c.textContent = st.text
  // Measure at the origin, not at the previous position.
  c.style.left = '0px'
  c.style.top = '0px'
  current = el
  document.addEventListener('keydown', onDocKey, true)
  if (c.hasAttribute('popover')) {
    try {
      c.showPopover()
    } catch {
      /* detached; positioning still applies */
    }
  } else c.style.display = 'block'
  void computePosition(el, c, { strategy: 'fixed', placement: 'top', middleware: [offset(6), flip({ padding: 8 }), shift({ padding: 8 })] }).then(({ x, y }) => {
    if (current !== el) return
    c.style.left = `${Math.round(x)}px`
    c.style.top = `${Math.round(y)}px`
  })
}

function schedule(el: HTMLElement) {
  clearTimeout(timer)
  timer = setTimeout(() => show(el), DELAY)
}

function bind(el: HTMLElement): () => void {
  const enter = (e: PointerEvent) => {
    if (e.pointerType === 'mouse' || e.pointerType === 'pen') schedule(el)
  }
  // Only the element's own focus: a wrapper must not steal the tooltip of a focusable child.
  const focus = (e: FocusEvent) => {
    if (e.target === el && el.matches(':focus-visible')) schedule(el)
  }
  const leave = (e: Event) => {
    if (e.type === 'focusout' && e.target !== el) return
    if (current === el || timer) hide()
  }
  el.addEventListener('pointerenter', enter)
  el.addEventListener('pointerleave', leave)
  el.addEventListener('pointerdown', leave)
  el.addEventListener('focusin', focus)
  el.addEventListener('focusout', leave)
  return () => {
    el.removeEventListener('pointerenter', enter)
    el.removeEventListener('pointerleave', leave)
    el.removeEventListener('pointerdown', leave)
    el.removeEventListener('focusin', focus)
    el.removeEventListener('focusout', leave)
  }
}

/** aria-describedby = the ids it had before + the hidden description (when it adds something). */
function syncDescription(el: HTMLElement, st: State) {
  st.descEl.textContent = st.text
  const useful = !!st.text && st.text !== el.getAttribute('aria-label')
  const ids = [st.origDescribedBy, useful ? st.descId : null].filter(Boolean).join(' ')
  if (ids) el.setAttribute('aria-describedby', ids)
  else el.removeAttribute('aria-describedby')
}

function apply(el: HTMLElement, value: Value, truncated: boolean) {
  let st = states.get(el)
  if (!st) {
    if (!descBox) {
      descBox = document.createElement('div')
      descBox.hidden = true
      document.body.appendChild(descBox)
    }
    const descEl = document.createElement('span')
    const descId = `kase-tip-${++seq}`
    descEl.id = descId
    descBox.appendChild(descEl)
    st = { text: '', truncated, cleanup: bind(el), descId, descEl, origDescribedBy: el.getAttribute('aria-describedby') }
    states.set(el, st)
  }
  st.text = value || ''
  st.truncated = truncated
  syncDescription(el, st)
  if (current === el) {
    if (!st.text) hide()
    else if (card) card.textContent = st.text
  }
}

if (typeof window !== 'undefined') {
  window.addEventListener('scroll', hide, true)
  window.addEventListener('blur', hide)
  window.addEventListener('resize', hide)
}

export const vTooltip: Directive<HTMLElement, Value> = {
  mounted: (el, { value, modifiers }) => apply(el, value, !!modifiers.truncated),
  updated: (el, { value, modifiers }) => apply(el, value, !!modifiers.truncated),
  unmounted(el) {
    const st = states.get(el)
    if (!st) return
    if (current === el) hide()
    st.cleanup()
    st.descEl.remove()
    if (st.origDescribedBy) el.setAttribute('aria-describedby', st.origDescribedBy)
    else el.removeAttribute('aria-describedby')
    states.delete(el)
  },
}
