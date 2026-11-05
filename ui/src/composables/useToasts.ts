// Pop-up notifications (ToastHost, mounted once in App): a short bold title and an optional muted
// detail line, green check for success, red alert for errors. They confirm something that already
// happened (a reply was sent); anything the person must act on stays inline in the view.
import { shallowRef } from 'vue'

export interface Toast {
  id: number
  tone: 'success' | 'error'
  title: string
  detail?: string
}

const SUCCESS_MS = 5000
const ERROR_MS = 9000
const MAX = 4

export const toasts = shallowRef<Toast[]>([])
let seq = 0
const timers = new Map<number, ReturnType<typeof setTimeout>>()

export function dismissToast(id: number) {
  clearTimeout(timers.get(id))
  timers.delete(id)
  toasts.value = toasts.value.filter((t) => t.id !== id)
}

export function showToast(t: Omit<Toast, 'id'>): number {
  const id = ++seq
  const list = [...toasts.value, { ...t, id }]
  for (const old of list.slice(0, Math.max(0, list.length - MAX))) dismissToast(old.id)
  toasts.value = list.slice(-MAX)
  timers.set(
    id,
    setTimeout(() => dismissToast(id), t.tone === 'error' ? ERROR_MS : SUCCESS_MS),
  )
  return id
}

/** Keeps a toast on screen while the pointer or keyboard focus is on it. */
export function holdToast(id: number) {
  clearTimeout(timers.get(id))
  timers.delete(id)
}

export function releaseToast(id: number) {
  if (!toasts.value.some((t) => t.id === id) || timers.has(id)) return
  timers.set(
    id,
    setTimeout(() => dismissToast(id), SUCCESS_MS),
  )
}
