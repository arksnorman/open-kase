// Horizontal overflow of a scroll box, kept current with a ResizeObserver on the box and its
// content. A box that scrolls sideways must be reachable by keyboard so it can be scrolled
// (WCAG 2.1.1; axe `scrollable-region-focusable`): callers make it focusable and name it while
// it overflows, and keep boxes whose content fits out of the tab order.
import { onBeforeUnmount, ref, watch, type Ref } from 'vue'

export function useOverflowX(el: Ref<HTMLElement | null>): Readonly<Ref<boolean>> {
  const overflowing = ref(false)
  let observer: ResizeObserver | null = null
  const measure = () => {
    const node = el.value
    overflowing.value = !!node && node.scrollWidth > node.clientWidth + 1
  }
  watch(
    el,
    (node) => {
      observer?.disconnect()
      observer = null
      if (!node) return
      measure()
      if (typeof ResizeObserver === 'undefined') return
      observer = new ResizeObserver(measure)
      observer.observe(node)
      if (node.firstElementChild) observer.observe(node.firstElementChild)
    },
    { immediate: true, flush: 'post' },
  )
  onBeforeUnmount(() => observer?.disconnect())
  return overflowing
}
