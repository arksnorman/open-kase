// A cursor-paged list: the first page comes from `useQuery` (cached, refetched on invalidation), further
// pages are appended on `loadMore` and dropped whenever the key changes or the first page is refetched.

import { computed, ref, shallowRef, watch, type MaybeRefOrGetter } from 'vue'
import { isAbortError } from '@/api/errors'
import type { Page } from '@/api/types'
import { anySignal, queryScopeSignal, useQuery } from './useQuery'

export function usePagedQuery<T>(
  key: MaybeRefOrGetter<string | null>,
  fetcher: (cursor: string | null, signal: AbortSignal) => Promise<Page<T>>,
) {
  const first = useQuery(key, (signal) => fetcher(null, signal))
  const more = shallowRef<T[]>([])
  const moreCursor = ref<string | null | undefined>(undefined)
  const loadingMore = ref(false)
  const moreError = ref<unknown>(null)
  const keyNow = () => (typeof key === 'function' ? key() : typeof key === 'string' || key === null ? key : key.value)

  let generation = 0
  function reset() {
    generation++
    loadingMore.value = false
    more.value = []
    moreCursor.value = undefined
    moreError.value = null
  }
  watch(keyNow, reset)
  // A new first page (refetch after a mutation) replaces everything loaded so far; a local `replace` does not.
  let localSet = false
  watch(first.data, () => {
    if (localSet) localSet = false
    else reset()
  })

  const items = computed(() => [...(first.data.value?.items ?? []), ...more.value])
  const nextCursor = computed(() => (moreCursor.value === undefined ? (first.data.value?.nextCursor ?? null) : moreCursor.value))

  async function loadMore() {
    const cursor = nextCursor.value
    if (!cursor || loadingMore.value) return
    const k = keyNow()
    const gen = generation
    loadingMore.value = true
    moreError.value = null
    try {
      const page = await fetcher(cursor, anySignal([queryScopeSignal()]))
      if (keyNow() !== k || gen !== generation) return
      more.value = [...more.value, ...page.items]
      moreCursor.value = page.nextCursor
    } catch (err) {
      if (!isAbortError(err) && gen === generation) moreError.value = err
    } finally {
      if (gen === generation) loadingMore.value = false
    }
  }

  /** Swap one loaded item for its updated version (e.g. a mutation response) without refetching. */
  function replace(item: T, same: (other: T) => boolean) {
    const d = first.data.value
    if (d && d.items.some(same)) {
      localSet = true
      first.set({ ...d, items: d.items.map((x) => (same(x) ? item : x)) })
    } else more.value = more.value.map((x) => (same(x) ? item : x))
  }

  return {
    items,
    replace,
    /** The first page was loaded (or failed). */
    ready: computed(() => !!first.data.value),
    loading: first.loading,
    error: first.error,
    moreError,
    hasMore: computed(() => !!nextCursor.value),
    loadingMore,
    loadMore,
    refetch: first.refetch,
  }
}
