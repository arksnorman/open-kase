// Reactive `matchMedia`: true while the media query matches. Layouts that depend on the
// viewport itself (the shell's sidebar or drawer) use this; layouts that depend on the space
// a component really gets use useElementWidth instead.
import { onBeforeUnmount, ref, type Ref } from 'vue'

export function useMediaQuery(query: string): Readonly<Ref<boolean>> {
  const mql = typeof window !== 'undefined' && window.matchMedia ? window.matchMedia(query) : null
  const matches = ref(mql?.matches ?? false)
  const onChange = (e: MediaQueryListEvent) => {
    matches.value = e.matches
  }
  mql?.addEventListener('change', onChange)
  onBeforeUnmount(() => mql?.removeEventListener('change', onChange))
  return matches
}

/** The shell shows its sidebar from this width; below it the navigation is a drawer. */
export const DESKTOP_SHELL_QUERY = '(min-width: 1024px)'
