// Client-side password guidance. The server is authoritative (field rule `user.password`:
// 12–256 characters); this only gives an early hint and never blocks a valid length.

export const PASSWORD_MIN = 12
export const PASSWORD_MAX = 256

export interface PasswordStrength {
  /** 0 = below the minimum, 1 weak, 2 fair, 3 good, 4 strong. */
  level: 0 | 1 | 2 | 3 | 4
  label: string
  hint: string
}

const LONG_HINT = 'A passphrase of several unrelated words is easy to type and hard to guess.'

export function passwordStrength(pw: string): PasswordStrength {
  const len = [...pw].length
  if (len < PASSWORD_MIN) {
    const more = PASSWORD_MIN - len
    return { level: 0, label: 'Too short', hint: `At least ${PASSWORD_MIN} characters (${more} more).` }
  }
  if (len > PASSWORD_MAX) return { level: 0, label: 'Too long', hint: `At most ${PASSWORD_MAX} characters.` }

  const classes = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^A-Za-z0-9]/].filter((r) => r.test(pw)).length
  const unique = new Set(pw.toLowerCase()).size
  let score = 1
  if (len >= 16) score++
  if (len >= 20) score++
  if (classes >= 3) score++
  // Repetition ("aaaaaaaaaaaa", "abcabcabcabc") makes a long password much weaker than it looks.
  if (unique < Math.min(8, len / 2)) score = 1

  const level = Math.min(4, score) as 1 | 2 | 3 | 4
  const labels = { 1: 'Weak', 2: 'Fair', 3: 'Good', 4: 'Strong' } as const
  return { level, label: labels[level], hint: level >= 3 ? 'Long enough to resist guessing.' : LONG_HINT }
}

/** Problem with a new password + confirmation before submitting, or null. */
export function newPasswordProblem(pw: string, confirm: string): string | null {
  const len = [...pw].length
  if (len < PASSWORD_MIN || len > PASSWORD_MAX) return `Use a password of ${PASSWORD_MIN} to ${PASSWORD_MAX} characters.`
  if (pw !== confirm) return 'The passwords do not match.'
  return null
}
