import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { authApi, meApi } from '@/api/endpoints'
import { setAccessToken, setSessionListener } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { AuthResponse, LoginResult, SecondFactor, User } from '@/api/types'
import { clearQueryCache } from '@/composables/useQuery'
import { usePreferencesStore } from './preferences'
import { useWorkspaceStore } from './workspace'

type Status = 'unknown' | 'authenticated' | 'anonymous'

/** A password was accepted but a second factor is needed before a session exists. */
export interface MfaChallenge {
  token: string
  expiresIn: number
}

export const useSessionStore = defineStore('session', () => {
  const user = shallowRef<User | null>(null)
  const status = ref<Status>('unknown')
  /** Set when the session could not be restored for a reason other than "not signed in". */
  const bootstrapError = ref<string | null>(null)
  /** Set when an active session ended unexpectedly (expired/revoked). */
  const expired = ref(false)
  /**
   * Administrator without MFA while the installation requires it: the API refuses everything but
   * the enrolment routes, so the router keeps the user on Account → Security until this clears.
   */
  const mfaEnrolmentRequired = ref(false)
  let bootstrapping: Promise<void> | null = null

  const isAuthenticated = computed(() => status.value === 'authenticated')
  const isAdmin = computed(() => user.value?.isAdmin === true)

  function adopt(auth: AuthResponse): void {
    user.value = auth.user
    status.value = 'authenticated'
    expired.value = false
    mfaEnrolmentRequired.value = auth.mfaEnrolmentRequired === true
    usePreferencesStore().adoptAccountTheme(auth.user.theme)
  }

  function clear(): void {
    setAccessToken(null)
    user.value = null
    status.value = 'anonymous'
    mfaEnrolmentRequired.value = false
    clearQueryCache()
    useWorkspaceStore().reset()
    usePreferencesStore().signedOut()
  }

  setSessionListener({
    onRefreshed: (auth) => {
      user.value = auth.user
      mfaEnrolmentRequired.value = auth.mfaEnrolmentRequired === true
    },
    onMfaEnrolmentRequired: () => {
      if (status.value === 'authenticated') mfaEnrolmentRequired.value = true
    },
    onSessionLost: () => {
      if (status.value === 'authenticated') expired.value = true
      clear()
    },
  })

  /** Restore a session from the refresh cookie once per page load. */
  function bootstrap(): Promise<void> {
    if (!bootstrapping) {
      bootstrapping = (async () => {
        try {
          const auth = await authApi.refresh()
          if (auth) {
            adopt(auth)
            if (!mfaEnrolmentRequired.value) await useWorkspaceStore().load()
          } else {
            status.value = 'anonymous'
          }
        } catch (err) {
          bootstrapError.value =
            err instanceof ApiError && err.status === 0
              ? 'The server could not be reached. Check your connection and try again.'
              : err instanceof ApiError && (err.status === 429 || err.status >= 500)
                ? 'The server is not available right now. Reload the page in a moment to continue.'
                : 'Your session could not be restored.'
          if (status.value !== 'authenticated') status.value = 'anonymous'
        }
      })()
    }
    return bootstrapping
  }

  async function signedIn(auth: AuthResponse): Promise<void> {
    bootstrapError.value = null
    adopt(auth)
    if (!mfaEnrolmentRequired.value) await useWorkspaceStore().load()
  }

  /** Password step. Resolves with a challenge when the account has MFA, else signs in. */
  /** `identifier` is an email address or a username. */
  async function login(identifier: string, password: string): Promise<MfaChallenge | null> {
    const res: LoginResult = await authApi.login(identifier, password)
    if (res.mfaRequired) {
      if (!res.mfaToken) throw new Error('The server did not return a sign-in challenge.')
      return { token: res.mfaToken, expiresIn: res.mfaExpiresIn ?? 300 }
    }
    if (!res.accessToken || !res.user) throw new Error('The server returned an incomplete sign-in response.')
    await signedIn({
      accessToken: res.accessToken,
      expiresIn: res.expiresIn ?? 0,
      mfaEnrolmentRequired: res.mfaEnrolmentRequired,
      user: res.user as User,
    })
    return null
  }

  /** Second step: TOTP code or recovery code for the challenge from `login`. */
  async function completeMfa(challenge: MfaChallenge, factor: SecondFactor): Promise<void> {
    await signedIn(await authApi.mfa(challenge.token, factor))
  }

  /** Re-reads the user (e.g. `mfaEnabled`) and lifts the enrolment gate once MFA is on. */
  async function refreshUser(): Promise<void> {
    const me = await meApi.get()
    user.value = me
    if (mfaEnrolmentRequired.value && me.mfaEnabled) {
      mfaEnrolmentRequired.value = false
      await useWorkspaceStore().load()
    }
  }

  async function logout(): Promise<void> {
    try {
      await authApi.logout()
    } catch {
      // The local session is cleared regardless; the server revokes on its next check.
    } finally {
      expired.value = false
      clear()
    }
  }

  /** A background check (e.g. the live-updates stream) found the session over. */
  function sessionEnded(): void {
    if (status.value === 'authenticated') expired.value = true
    clear()
  }

  function setUser(next: User): void {
    user.value = next
  }

  return {
    user,
    status,
    bootstrapError,
    expired,
    mfaEnrolmentRequired,
    isAuthenticated,
    isAdmin,
    bootstrap,
    login,
    completeMfa,
    refreshUser,
    logout,
    setUser,
    sessionEnded,
  }
})
