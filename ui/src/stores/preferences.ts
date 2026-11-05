import { defineStore } from 'pinia'
import { ref } from 'vue'
import { meApi } from '@/api/endpoints'
import type { ThemeChoice } from '@/api/types'
import { applyTheme, readStoredTheme, storeTheme, watchSystemTheme } from '@/theme/theme'

export const usePreferencesStore = defineStore('preferences', () => {
  const theme = ref<ThemeChoice>(readStoredTheme())
  const syncError = ref<string | null>(null)
  let authenticated = false

  watchSystemTheme(() => theme.value)

  function setLocal(choice: ThemeChoice): void {
    theme.value = choice
    storeTheme(choice)
    applyTheme(choice)
  }

  /** User-initiated change: apply now, persist to the account when signed in. */
  async function setTheme(choice: ThemeChoice): Promise<void> {
    setLocal(choice)
    if (!authenticated) return
    try {
      syncError.value = null
      await meApi.setTheme(choice)
    } catch {
      syncError.value = 'Theme saved on this device only; the account preference was not updated.'
    }
  }

  /** After sign-in or session restore the account preference wins over the device fallback. */
  function adoptAccountTheme(choice: ThemeChoice): void {
    authenticated = true
    if (choice !== theme.value) setLocal(choice)
  }

  function signedOut(): void {
    authenticated = false
  }

  return { theme, syncError, setTheme, adoptAccountTheme, signedOut }
})
