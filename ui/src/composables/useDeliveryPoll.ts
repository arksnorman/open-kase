// Polls one delivery record until it reaches a terminal state (or a time limit), for
// "send test" actions. Stops on unmount or when a new poll starts.
import { onScopeDispose, ref, shallowRef } from 'vue'
import { isAbortError } from '@/api/errors'
import type { Delivery } from '@/api/types'
import { isTerminal } from '@/deliveries'

const LIMIT_MS = 2 * 60 * 1000

export function useDeliveryPoll() {
  const delivery = shallowRef<Delivery | null>(null)
  const polling = ref(false)
  /** Polling stopped while the delivery was still pending/retrying. */
  const gaveUp = ref(false)
  const error = ref<unknown>(null)
  let ctrl: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | null = null

  function stop(): void {
    ctrl?.abort()
    ctrl = null
    if (timer) clearTimeout(timer)
    timer = null
    polling.value = false
  }

  function start(initial: Delivery, fetch: (id: string, signal: AbortSignal) => Promise<Delivery>): void {
    stop()
    delivery.value = initial
    gaveUp.value = false
    error.value = null
    if (isTerminal(initial.status)) return
    const own = new AbortController()
    ctrl = own
    polling.value = true
    const started = Date.now()
    let delay = 1000
    const tick = async () => {
      try {
        const d = await fetch(initial.id, own.signal)
        if (own.signal.aborted) return
        delivery.value = d
        if (isTerminal(d.status)) return stop()
      } catch (err) {
        if (isAbortError(err) || own.signal.aborted) return
        error.value = err
        return stop()
      }
      if (Date.now() - started > LIMIT_MS) {
        gaveUp.value = true
        return stop()
      }
      delay = Math.min(delay * 1.5, 5000)
      timer = setTimeout(tick, delay)
    }
    timer = setTimeout(tick, delay)
  }

  function reset(): void {
    stop()
    delivery.value = null
    gaveUp.value = false
    error.value = null
  }

  onScopeDispose(stop)
  return { delivery, polling, gaveUp, error, start, stop, reset }
}
