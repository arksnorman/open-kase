// A tab left open across a deploy asks for code-split files the new build no longer has, so a
// lazy route or component fails to import. Nothing is wrong with the user's work, so this is
// reported as a non-blocking banner ("Reload to continue") rather than an error page, and the
// page is never reloaded automatically: that could throw away an unsent draft.
import { ref } from 'vue'

export const staleBuild = ref(false)
/** Where the failed navigation was heading; Reload goes there instead of the current page. */
let reloadTarget: string | null = null

/** Browsers word this differently and none uses a distinct error type; the message is all there is. */
export function isChunkLoadError(err: unknown): boolean {
  const message = err instanceof Error ? err.message : typeof err === 'string' ? err : ''
  const name = err instanceof Error ? err.name : ''
  return (
    name === 'ChunkLoadError' ||
    /dynamically imported module|importing a module script failed|error loading dynamically imported|unable to preload css|failed to load module script/i.test(message)
  )
}

export function markStaleBuild(target?: string): void {
  if (target) reloadTarget = target
  staleBuild.value = true
}

export function reloadApp(): void {
  if (reloadTarget && reloadTarget !== window.location.pathname + window.location.search + window.location.hash) window.location.assign(reloadTarget)
  else window.location.reload()
}

let listening = false
/** Vite dispatches `vite:preloadError` when a dynamic import's preloads fail (CSS or JS). */
export function watchForStaleBuild(): void {
  if (listening) return
  listening = true
  window.addEventListener('vite:preloadError', () => markStaleBuild())
  window.addEventListener('unhandledrejection', (e) => {
    if (isChunkLoadError(e.reason)) markStaleBuild()
  })
}
