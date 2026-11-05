// Live updates: Server-Sent Events from GET /events read
// with fetch streaming, so the access token travels in the Authorization header (EventSource
// cannot send it, and a token in the URL is refused by the server). Events only say *what*
// changed; views refetch through the normal endpoints, which apply authorization again.
//
// - One stream per signed-in tab, owned by the agent workspace frame (`startLiveUpdates`).
// - Reconnects with exponential backoff and jitter; refreshes the access token when the
//   server ends the stream because the credential expired.
// - While the stream is unavailable, polls (a refetch of everything shown) every 30 s.
// - Stops on sign-out and when the frame unmounts.
// - There is no replay: after every (re)connect the stream is opened first, then data is refetched.

import { onScopeDispose, readonly, ref, watch } from 'vue'
import { apiUrl, authHeaders, refreshSession, responseError } from '@/api/client'
import { isAbortError } from '@/api/errors'
import type { LiveInvalidate } from '@/api/types'
import { useWorkspaceStore } from '@/stores/workspace'
import { useSessionStore } from '@/stores/session'
import { invalidate } from './useQuery'

export type LiveState = 'off' | 'connecting' | 'live' | 'reconnecting'

export type LiveEvent =
  | { kind: 'invalidate'; event: LiveInvalidate }
  /** Events may have been missed (reconnect, server overflow, polling tick): refetch everything shown. */
  | { kind: 'resync' }

const POLL_MS = 30_000
const BACKOFF_MIN_MS = 1_000
const BACKOFF_MAX_MS = 60_000
const LIST_DEBOUNCE_MS = 300

const state = ref<LiveState>('off')
/** True while the 30 s fallback polling runs (stream unavailable). */
const polling = ref(false)
const handlers = new Set<(e: LiveEvent) => void>()

/** Read-only connection state for the status indicator. */
export function useLiveState() {
  return { state: readonly(state), polling: readonly(polling) }
}

/** Subscribe to live events for the lifetime of the calling component/effect scope. */
export function onLiveEvent(handler: (e: LiveEvent) => void): void {
  handlers.add(handler)
  onScopeDispose(() => handlers.delete(handler))
}

function dispatch(e: LiveEvent) {
  for (const h of handlers) {
    try {
      h(e)
    } catch {
      // a broken handler must not stop the stream
    }
  }
}

interface SseMessage {
  event: string
  data: string
  id: string
}

/** Incremental `text/event-stream` parser (WHATWG rules for the fields we use). */
function createParser(onMessage: (m: SseMessage) => void) {
  let buffer = ''
  let event = ''
  let data: string[] = []
  let id = ''
  function line(l: string) {
    if (l === '') {
      if (data.length || event) onMessage({ event: event || 'message', data: data.join('\n'), id })
      event = ''
      data = []
      return
    }
    if (l.startsWith(':')) return // comment (": ping")
    const i = l.indexOf(':')
    const field = i === -1 ? l : l.slice(0, i)
    let value = i === -1 ? '' : l.slice(i + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') event = value
    else if (field === 'data') data.push(value)
    else if (field === 'id') id = value
  }
  return (chunk: string) => {
    buffer += chunk
    let nl: number
    while ((nl = buffer.search(/\r\n|\r|\n/)) !== -1) {
      const l = buffer.slice(0, nl)
      buffer = buffer.slice(nl + (buffer[nl] === '\r' && buffer[nl + 1] === '\n' ? 2 : 1))
      line(l)
    }
  }
}

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    if (signal.aborted) return resolve()
    const t = setTimeout(resolve, ms)
    signal.addEventListener('abort', () => (clearTimeout(t), resolve()), { once: true })
  })

type StreamEnd =
  | { kind: 'closed' } // network drop, server end, watchdog
  | { kind: 'reconnect' } // server shutdown notice
  | { kind: 'auth'; token: string } // credential expired/revoked
  | { kind: 'forbidden' } // access lost

/** Runs the stream (with fallback polling) until `signal` aborts. */
async function runStream(
  signal: AbortSignal,
  lost: () => void,
  sessionEnded: () => void,
): Promise<void> {
  let attempt = 0
  let connectedBefore = false
  let pollTimer: ReturnType<typeof setInterval> | undefined
  let listTimer: ReturnType<typeof setTimeout> | undefined

  const invalidateLists = () => {
    clearTimeout(listTimer)
    listTimer = setTimeout(() => {
      invalidate('tickets:')
      invalidate('queue-counts')
    }, LIST_DEBOUNCE_MS)
  }
  const resync = () => {
    invalidateLists()
    dispatch({ kind: 'resync' })
  }
  const startPolling = () => {
    if (pollTimer) return
    polling.value = true
    pollTimer = setInterval(resync, POLL_MS)
  }
  const stopPolling = () => {
    clearInterval(pollTimer)
    pollTimer = undefined
    polling.value = false
  }
  signal.addEventListener('abort', () => (stopPolling(), clearTimeout(listTimer)), { once: true })

  while (!signal.aborted) {
    state.value = attempt === 0 && !connectedBefore ? 'connecting' : 'reconnecting'
    const headers = authHeaders()
    const token = headers.Authorization ?? ''
    const conn = new AbortController()
    const abortConn = () => conn.abort()
    signal.addEventListener('abort', abortConn, { once: true })
    let end: StreamEnd = { kind: 'closed' }
    try {
      const res = await fetch(apiUrl('/events'), {
        method: 'GET',
        headers: { Accept: 'text/event-stream', ...headers },
        credentials: 'same-origin',
        cache: 'no-store',
        signal: conn.signal,
      })
      if (res.status === 401) end = { kind: 'auth', token }
      else if (res.status === 403 || res.status === 404) end = { kind: 'forbidden' }
      else if (!res.ok || !res.body) {
        await responseError(res).catch(() => undefined)
      } else {
        end = await consume(res.body, conn, () => {
          attempt = 0
          state.value = 'live'
          stopPolling()
          // Contract: open the stream first, then refetch. The first connect follows the
          // initial page load, so only reconnects need the refetch.
          if (connectedBefore) resync()
          connectedBefore = true
        }, (e) => {
          invalidateLists()
          dispatch({ kind: 'invalidate', event: e })
        }, resync, token)
      }
    } catch (err) {
      if (!isAbortError(err) && !signal.aborted) end = { kind: 'closed' }
    } finally {
      signal.removeEventListener('abort', abortConn)
    }
    if (signal.aborted) break

    if (end.kind === 'forbidden') {
      // Access changed (e.g. group memberships): reload the user's groups, keep polling and
      // retry slowly.
      lost()
      attempt = Math.max(attempt, 6)
    }
    if (end.kind === 'auth') {
      // Another request may already have rotated the token; only refresh if not.
      if (authHeaders().Authorization === end.token) {
        try {
          const auth = await refreshSession()
          if (!auth) {
            state.value = 'off'
            sessionEnded()
            return
          }
        } catch {
          // network failure: fall through to backoff
        }
      }
      if (attempt === 0) {
        attempt = 1
        continue // reconnect at once with the new token
      }
    }

    state.value = 'reconnecting'
    startPolling()
    const base = end.kind === 'reconnect' ? 2_000 : Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** attempt)
    attempt++
    await sleep(base / 2 + Math.random() * (base / 2), signal)
  }
}

async function consume(
  body: ReadableStream<Uint8Array>,
  conn: AbortController,
  onReady: () => void,
  onInvalidate: (e: LiveInvalidate) => void,
  onResync: () => void,
  token: string,
): Promise<StreamEnd> {
  let end: StreamEnd = { kind: 'closed' }
  // Watchdog: the server sends a ping every heartbeat; silence means a dead connection.
  let heartbeatMs = 15_000
  let watchdog: ReturnType<typeof setTimeout> | undefined
  const arm = () => {
    clearTimeout(watchdog)
    watchdog = setTimeout(() => conn.abort(), heartbeatMs * 2 + 5_000)
  }
  const parse = createParser((m) => {
    let data: Record<string, unknown> = {}
    try {
      data = m.data ? (JSON.parse(m.data) as Record<string, unknown>) : {}
    } catch {
      return
    }
    switch (m.event) {
      case 'ready':
        if (typeof data.heartbeatSeconds === 'number' && data.heartbeatSeconds > 0) heartbeatMs = data.heartbeatSeconds * 1000
        arm()
        onReady()
        break
      case 'invalidate':
        if (typeof data.ticketId === 'string' && typeof data.type === 'string')
          onInvalidate({ type: data.type, ticketId: data.ticketId, version: typeof data.version === 'number' ? data.version : 0 })
        break
      case 'resync':
        onResync()
        break
      case 'error': {
        const code = typeof data.code === 'string' ? data.code : ''
        // Lost access (`forbidden`, `projectNotFound`, `insufficientScope`) vs an expired or
        // revoked credential (`tokenExpired`, `sessionRevoked`, `accountInactive`, …).
        end = /forbidden|notFound|insufficientScope/.test(code) ? { kind: 'forbidden' } : { kind: 'auth', token }
        conn.abort()
        break
      }
      case 'reconnect':
        end = { kind: 'reconnect' }
        conn.abort()
        break
    }
  })
  const reader = body.getReader()
  const decoder = new TextDecoder()
  arm()
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      arm()
      parse(decoder.decode(value, { stream: true }))
    }
  } catch (err) {
    if (!isAbortError(err) && !(err instanceof TypeError)) throw err
  } finally {
    clearTimeout(watchdog)
    reader.releaseLock?.()
  }
  return end
}

/**
 * Owns the stream. Call once from the agent workspace frame; it follows sign-out and stops when
 * the frame unmounts.
 */
export function startLiveUpdates(): void {
  const workspace = useWorkspaceStore()
  const session = useSessionStore()
  let ctrl: AbortController | null = null

  const stop = () => {
    ctrl?.abort()
    ctrl = null
    state.value = 'off'
    polling.value = false
  }

  watch(
    () => session.isAuthenticated,
    (authed) => {
      stop()
      if (!authed) return
      const own = new AbortController()
      ctrl = own
      void runStream(
        own.signal,
        () => {
          if (!own.signal.aborted) void workspace.load().catch(() => undefined)
        },
        () => session.sessionEnded(),
      )
    },
    { immediate: true },
  )
  onScopeDispose(stop)
}
