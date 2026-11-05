import { ApiError } from '@/api/errors'

// Mirrors the server rule `user.username` (checked after trimming and lower-casing).
const USERNAME_RE = /^[a-z][a-z0-9._-]{2,31}$/
export const USERNAME_HINT = "3 to 32 lower-case letters, digits, '.', '_' or '-', starting with a letter."
export const normUsername = (v: string) => v.trim().toLowerCase()

/** Client-side hint only; the server validates again. */
export function usernameProblem(v: string): string | undefined {
  const n = normUsername(v)
  if (!n) return undefined
  if (n.includes('@')) return 'A username cannot be an email address.'
  return USERNAME_RE.test(n) ? undefined : `Use ${USERNAME_HINT}`
}

export const usernameTaken = (err: unknown) =>
  err instanceof ApiError && err.is('usernameTaken') ? 'This username is already in use.' : undefined

/** The username the server derives from an email address when none is given (it may add a number when taken). */
export function derivedUsername(email: string): string {
  let local = email.trim().toLowerCase()
  const at = local.lastIndexOf('@')
  if (at >= 0) local = local.slice(0, at)
  let base = ''
  for (const c of local) {
    if (c >= 'a' && c <= 'z') base += c
    else if (base && /[0-9._-]/.test(c)) base += c
  }
  if (base.length < 3) base = base ? `user-${base}` : 'user'
  return base.slice(0, 32)
}

export const userStatusLabel = (s: 'active' | 'disabled' | 'invited') => (s === 'active' ? 'Active' : s === 'invited' ? 'Invited' : 'Disabled')
