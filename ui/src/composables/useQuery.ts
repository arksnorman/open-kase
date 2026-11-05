// Minimal server-state layer: keyed in-memory cache (never persisted), abort on key change,
// abort on sign-out, stale-result protection.

import { onScopeDispose, ref, shallowRef, toValue, watch, type MaybeRefOrGetter, type Ref, type ShallowRef } from 'vue'
import { isAbortError } from '@/api/errors'

const cache = new Map<string, unknown>()
const listeners = new Set<(prefix: string) => void>()

/** Drop cached entries whose key starts with `prefix` and refetch active queries. */
export function invalidate(prefix: string): void {
  for (const k of cache.keys()) if (k.startsWith(prefix)) cache.delete(k)
  for (const l of listeners) l(prefix)
}

// Requests share this controller; sign-out aborts it so a slow response for the old session can
// never populate the next one.
let scope = new AbortController()

/** Signal for manual requests (e.g. "load more") that must stop on sign-out. */
export function queryScopeSignal(): AbortSignal {
  return scope.signal
}

export function clearQueryCache(): void {
  scope.abort()
  scope = new AbortController()
  cache.clear()
}

export function anySignal(signals: AbortSignal[]): AbortSignal {
  if (typeof AbortSignal.any === 'function') return AbortSignal.any(signals)
  const ctrl = new AbortController()
  for (const s of signals) {
    if (s.aborted) {
      ctrl.abort()
      break
    }
    s.addEventListener('abort', () => ctrl.abort(), { once: true })
  }
  return ctrl.signal
}

export interface QueryResult<T> {
  data: ShallowRef<T | undefined>
  error: Ref<unknown>
  loading: Ref<boolean>
  refetch: () => Promise<void>
  /** Replace the data locally (e.g. with a mutation response) and update the cache. */
  set: (value: T) => void
}

export function useQuery<T>(
  key: MaybeRefOrGetter<string | null>,
  fetcher: (signal: AbortSignal) => Promise<T>,
): QueryResult<T> {
  const data = shallowRef<T | undefined>(undefined)
  const error = ref<unknown>(null)
  const loading = ref(false)
  let ctrl: AbortController | null = null
  let activeKey: string | null = null

  async function run(k: string | null): Promise<void> {
    ctrl?.abort()
    ctrl = null
    activeKey = k
    if (k === null) {
      data.value = undefined
      loading.value = false
      return
    }
    const own = new AbortController()
    ctrl = own
    data.value = cache.get(k) as T | undefined
    error.value = null
    loading.value = true
    try {
      const result = await fetcher(anySignal([own.signal, scope.signal]))
      if (activeKey !== k || own.signal.aborted) return
      cache.set(k, result)
      data.value = result
    } catch (err) {
      if (isAbortError(err) || activeKey !== k) return
      error.value = err
    } finally {
      if (activeKey === k && ctrl === own) loading.value = false
    }
  }

  watch(() => toValue(key), (k) => void run(k), { immediate: true })

  const onInvalidate = (prefix: string) => {
    if (activeKey && activeKey.startsWith(prefix)) void run(activeKey)
  }
  listeners.add(onInvalidate)
  onScopeDispose(() => {
    listeners.delete(onInvalidate)
    ctrl?.abort()
  })

  return {
    data,
    error,
    loading,
    refetch: () => run(toValue(key)),
    set: (value: T) => {
      data.value = value
      if (activeKey) cache.set(activeKey, value)
    },
  }
}
