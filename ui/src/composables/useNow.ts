// One shared minute ticker for every relative timestamp on the page.
import { onScopeDispose, readonly, ref } from 'vue'

const now = ref(Date.now())
let users = 0
let timer: ReturnType<typeof setInterval> | undefined

export function useNow() {
  users++
  if (users === 1) {
    now.value = Date.now()
    timer = setInterval(() => (now.value = Date.now()), 60_000)
  }
  onScopeDispose(() => {
    users--
    if (users === 0 && timer) {
      clearInterval(timer)
      timer = undefined
    }
  })
  return readonly(now)
}
