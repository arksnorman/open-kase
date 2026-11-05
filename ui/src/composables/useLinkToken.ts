import { useRoute, useRouter } from 'vue-router'

/**
 * Reads a single-use `?token=` from an emailed link once, then removes it from the address bar
 * and the history entry (so it is not left in history, bookmarks, screenshots or a Referer).
 * The token stays only in the calling component's memory; never log it.
 */
export function useLinkToken(): string | null {
  const route = useRoute()
  const router = useRouter()
  const raw = route.query.token
  const token = typeof raw === 'string' && raw.trim() ? raw.trim() : null
  if ('token' in route.query) {
    const { token: _drop, ...rest } = route.query
    void _drop
    const params = new URLSearchParams()
    for (const [k, v] of Object.entries(rest)) if (typeof v === 'string') params.set(k, v)
    const qs = params.toString()
    const clean = `${window.location.pathname}${qs ? `?${qs}` : ''}${window.location.hash}`
    // Synchronously first; vue-router keeps the full path in history.state.current too.
    const state = (window.history.state ?? {}) as Record<string, unknown>
    window.history.replaceState({ ...state, current: clean }, '', clean)
    void router.replace({ query: rest, hash: route.hash })
  }
  return token
}
