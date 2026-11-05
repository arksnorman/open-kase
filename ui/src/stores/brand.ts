import { defineStore } from 'pinia'
import { ref } from 'vue'
import { product } from '@/product'

/**
 * The configured product name (settings `product.name`). Starts as the built-in fallback, is filled from the
 * unauthenticated `GET /meta` on startup (so the sign-in pages show it) and refreshed from the project once loaded.
 */
export const useBrandStore = defineStore('brand', () => {
  const name = ref<string>(product.name)
  /** False until the name is known (or the lookup failed), so pages can avoid flashing the fallback. */
  const ready = ref(false)
  let fetched = false

  function set(next: string | null | undefined): void {
    const n = next?.trim()
    if (n) {
      name.value = n
      ready.value = true
    }
  }

  async function load(): Promise<void> {
    if (fetched) return
    fetched = true
    try {
      const res = await fetch(`${product.apiBase}/meta`, { headers: { Accept: 'application/json' } })
      if (res.ok) set(((await res.json()) as { product?: string }).product)
      else fetched = false
    } catch {
      fetched = false
    } finally {
      ready.value = true
    }
  }

  return { name, ready, set, load }
})
