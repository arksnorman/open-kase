// Inner width of an element, kept current with a ResizeObserver. Layouts that depend on the
// space a component really gets (the workspace beside the sidebar, a panel beside a thread)
// read this instead of the viewport width, so they keep working when the frame around them
// changes. 0 until the element is mounted.
import { onBeforeUnmount, ref, watch, type Ref } from 'vue'

export function useElementWidth(el: Ref<HTMLElement | null>): Readonly<Ref<number>> {
  const width = ref(0)
  let observer: ResizeObserver | null = null

  watch(
    el,
    (node) => {
      observer?.disconnect()
      observer = null
      if (!node) return
      width.value = node.clientWidth
      if (typeof ResizeObserver === 'undefined') return
      observer = new ResizeObserver(() => {
        width.value = node.clientWidth
      })
      observer.observe(node)
    },
    { immediate: true, flush: 'post' },
  )
  onBeforeUnmount(() => observer?.disconnect())

  return width
}
