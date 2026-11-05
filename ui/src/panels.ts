// Collapsible panels (navigation sidebar `[`, ticket Details/Activity panel `]`): the remembered
// state is a per-browser convenience in localStorage, so every access is guarded (private windows
// and blocked storage throw) and the UI works without it.

export function readStored(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

export function writeStored(key: string, value: string) {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // not remembered; the panel still works
  }
}

/** Single-key shortcuts are ignored while the person is typing (inputs, selects, rich-text editors). */
export function isTypingTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el || !el.tagName) return false
  return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable
}
