// Appearance handling. The pre-paint inline script in index.html mirrors applyTheme();
// keep the storage key and attribute names in sync.

import type { ThemeChoice } from '@/api/types'

export const THEME_STORAGE_KEY = 'oc.theme'
const media = typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: dark)') : null

export function isThemeChoice(v: unknown): v is ThemeChoice {
  return v === 'system' || v === 'light' || v === 'dark'
}

export function readStoredTheme(): ThemeChoice {
  try {
    const v = localStorage.getItem(THEME_STORAGE_KEY)
    return isThemeChoice(v) ? v : 'system'
  } catch {
    return 'system'
  }
}

export function storeTheme(choice: ThemeChoice): void {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, choice)
  } catch {
    // storage unavailable (private mode); the in-memory choice still applies
  }
}

export function resolveTheme(choice: ThemeChoice): 'light' | 'dark' {
  if (choice === 'system') return media?.matches ? 'dark' : 'light'
  return choice
}

export function applyTheme(choice: ThemeChoice): void {
  const resolved = resolveTheme(choice)
  const root = document.documentElement
  root.setAttribute('data-theme', resolved)
  root.setAttribute('data-theme-choice', choice)
  root.style.colorScheme = resolved
}

/** Re-apply when the OS scheme changes while the choice is "system". */
export function watchSystemTheme(current: () => ThemeChoice): void {
  media?.addEventListener('change', () => {
    if (current() === 'system') applyTheme('system')
  })
}
