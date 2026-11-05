// Floating panels (select listboxes, popovers, menus) anchored to a trigger.
//
// Positioning: @floating-ui/dom with the `fixed` strategy, so a panel escapes `overflow`
// clipping of scroll containers, tables and dialog bodies; `flip` opens it upwards when there
// is no room below and `shift` keeps it on screen. `size` caps the height to the space
// available (the list scrolls instead of running off the viewport) and can match the
// trigger's width. `autoUpdate` follows scrolling, resizing and layout shifts while open.
//
// Layering: where the browser supports the Popover API the panel is shown with
// `popover="manual"`, which puts it in the top layer above everything, including a native
// modal <dialog> (BaseModal). That is the reason it is not teleported to <body>: a
// teleported panel would render *underneath* an open modal dialog. The panel stays a DOM
// descendant of its component, so outside-click checks and focus handling stay local.
import { autoUpdate, computePosition, flip, offset, shift, size, type Placement } from '@floating-ui/dom'
import { nextTick, onBeforeUnmount, ref, watch, type Ref } from 'vue'

export const supportsPopover = typeof HTMLElement !== 'undefined' && 'showPopover' in HTMLElement.prototype

export interface FloatingOptions {
  placement?: () => Placement
  /** Panel at least as wide as the trigger (`min`) or exactly as wide (`exact`). */
  matchWidth?: () => 'min' | 'exact' | false
  /** Gap between trigger and panel, px. */
  gap?: number
  /** Upper bound for the panel height, px; also limited by the space available. */
  maxHeight?: number
}

export function useFloating(reference: Ref<HTMLElement | null>, floating: Ref<HTMLElement | null>, open: Ref<boolean>, opts: FloatingOptions = {}) {
  const style = ref<Record<string, string>>({ position: 'fixed', left: '0px', top: '0px' })
  const actualPlacement = ref<Placement>(opts.placement?.() ?? 'bottom-start')
  let stop: (() => void) | null = null

  async function update() {
    const ref_ = reference.value
    const el = floating.value
    if (!ref_ || !el) return
    const match = opts.matchWidth?.() ?? false
    const cap = opts.maxHeight
    const { x, y, placement } = await computePosition(ref_, el, {
      strategy: 'fixed',
      placement: opts.placement?.() ?? 'bottom-start',
      middleware: [
        offset(opts.gap ?? 4),
        flip({ padding: 8, fallbackAxisSideDirection: 'start' }),
        shift({ padding: 8 }),
        size({
          padding: 8,
          apply({ availableHeight, availableWidth, rects }) {
            const h = Math.max(120, Math.floor(availableHeight))
            el.style.maxHeight = `${cap ? Math.min(cap, h) : h}px`
            el.style.maxWidth = `${Math.max(160, Math.floor(availableWidth))}px`
            if (match === 'exact') el.style.width = `${rects.reference.width}px`
            else if (match === 'min') el.style.minWidth = `${rects.reference.width}px`
          },
        }),
      ],
    })
    actualPlacement.value = placement
    style.value = { position: 'fixed', left: `${Math.round(x)}px`, top: `${Math.round(y)}px` }
  }

  function start() {
    const el = floating.value
    const ref_ = reference.value
    if (!el || !ref_) return
    if (supportsPopover && el.hasAttribute('popover') && !el.matches(':popover-open')) {
      try {
        el.showPopover()
      } catch {
        /* already shown or detached; positioning still applies */
      }
    }
    stop?.()
    stop = autoUpdate(ref_, el, () => void update())
  }

  function teardown() {
    stop?.()
    stop = null
  }

  watch(
    open,
    async (v) => {
      if (!v) return teardown()
      await nextTick()
      start()
    },
    { immediate: true, flush: 'post' },
  )
  onBeforeUnmount(teardown)

  return { style, placement: actualPlacement, update }
}
