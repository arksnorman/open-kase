// Fetch wrapper for /api/v1. The access token lives only in this module's memory; the
// refresh token is an HttpOnly cookie the browser sends to /api/v1/auth/* by itself.

import { product } from '@/product'
import { ApiError, type ProblemDetails } from './errors'
import type { AuthResponse } from './types'

const BASE = product.apiBase
const CSRF_COOKIE = 'kase_csrf'

let accessToken: string | null = null
let refreshInFlight: Promise<AuthResponse | null> | null = null

type SessionListener = {
  onRefreshed?: (auth: AuthResponse) => void
  onSessionLost?: () => void
  /** A route answered `403 mfaEnrolmentRequired` (admin MFA policy turned on mid-session). */
  onMfaEnrolmentRequired?: () => void
}
let listener: SessionListener = {}

/** Called with every failed API response that carries problem `actions` (e.g. create follow-up). */
let problemHook: ((err: ApiError) => void) | null = null
export function setProblemActionHook(hook: ((err: ApiError) => void) | null): void {
  problemHook = hook
}

/** The session store registers here so the client can report rotations and expiry. */
export function setSessionListener(l: SessionListener): void {
  listener = l
}

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function hasAccessToken(): boolean {
  return accessToken !== null
}

export function readCookie(name: string): string | null {
  for (const part of document.cookie.split(';')) {
    const [k, ...rest] = part.trim().split('=')
    if (k === name) return decodeURIComponent(rest.join('='))
  }
  return null
}

function csrfHeaders(): Record<string, string> {
  const token = readCookie(CSRF_COOKIE)
  return token ? { 'X-CSRF-Token': token } : {}
}

/** Random key for Idempotency-Key headers. Works outside secure contexts (e.g. LAN dev). */
export function newIdempotencyKey(): string {
  const c = globalThis.crypto
  if (typeof c.randomUUID === 'function' && globalThis.isSecureContext) return c.randomUUID()
  const bytes = c.getRandomValues(new Uint8Array(16))
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

export interface RequestOptions {
  query?: Record<string, string | number | boolean | null | undefined>
  body?: unknown
  signal?: AbortSignal
  /** Sends `If-Match: <version>` for optimistic concurrency. */
  ifMatch?: number
  /** Sends `Idempotency-Key`; pass `true` to generate one. */
  idempotencyKey?: string | true
  /** Defaults to true. Auth endpoints set false. */
  auth?: boolean
  /** Adds credentials + X-CSRF-Token for cookie-authenticated routes. */
  cookie?: boolean
  headers?: Record<string, string>
}

function buildUrl(path: string, query?: RequestOptions['query']): string {
  let url = BASE + path
  if (query) {
    const params = new URLSearchParams()
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined && v !== null && v !== '') params.set(k, String(v))
    }
    const qs = params.toString()
    if (qs) url += `?${qs}`
  }
  return url
}

async function toApiError(res: Response): Promise<ApiError> {
  let problem: ProblemDetails = {}
  const type = res.headers.get('content-type') ?? ''
  if (type.includes('json')) {
    try {
      problem = (await res.json()) as ProblemDetails
    } catch {
      // body was not valid JSON; fall back to status only
    }
  } else {
    // Not a problem document (e.g. a proxy's error page): release the connection.
    void res.body?.cancel().catch(() => {})
  }
  if (!problem.requestId) {
    const rid = res.headers.get('x-request-id')
    if (rid) problem.requestId = rid
  }
  return new ApiError(res.status, problem, retryAfterSeconds(res.headers.get('retry-after')))
}

/** Retry-After as seconds (delta-seconds or an HTTP date), or null. */
function retryAfterSeconds(v: string | null): number | null {
  if (!v) return null
  const s = /^\s*\d+\s*$/.test(v) ? Number(v) : Math.ceil((Date.parse(v) - Date.now()) / 1000)
  return Number.isFinite(s) && s >= 0 ? s : null
}

/** Headers for a streaming request made outside `rawRequest` (SSE over fetch). Never puts the token in the URL. */
export function authHeaders(): Record<string, string> {
  return accessToken ? { Authorization: `Bearer ${accessToken}` } : {}
}

export { buildUrl as apiUrl, toApiError as responseError }

async function send(method: string, path: string, opts: RequestOptions): Promise<Response> {
  const headers: Record<string, string> = { Accept: 'application/json', ...opts.headers }
  let body: BodyInit | undefined
  if (opts.body instanceof FormData) {
    body = opts.body
  } else if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  if (opts.auth !== false && accessToken) headers.Authorization = `Bearer ${accessToken}`
  if (opts.ifMatch !== undefined) headers['If-Match'] = String(opts.ifMatch)
  if (opts.idempotencyKey) {
    headers['Idempotency-Key'] = opts.idempotencyKey === true ? newIdempotencyKey() : opts.idempotencyKey
  }
  if (opts.cookie) Object.assign(headers, csrfHeaders())

  const init: RequestInit = {
    method,
    headers,
    credentials: opts.cookie ? 'include' : 'same-origin',
    cache: 'no-store',
  }
  if (body !== undefined) init.body = body
  if (opts.signal) init.signal = opts.signal

  try {
    return await fetch(buildUrl(path, opts.query), init)
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, { title: 'Network error', detail: 'The server could not be reached.' })
  }
}

/** A refresh answer that says nothing about the session itself (retry later). */
function isTransient(status: number): boolean {
  return status === 429 || status >= 500
}

/**
 * Exchange the refresh cookie for a new access token. Concurrent callers share one request
 * so a burst of 401s cannot trigger a refresh storm (or refresh-token reuse detection).
 * Resolves null when there is no valid session; rejects (keeping the session) on a network
 * failure or a transient server answer (429, 5xx). Several tabs refreshing at once are covered
 * by the server's grace window: a refresh inside it returns an access token and sets no
 * cookie, which needs no handling here.
 */
export function refreshSession(): Promise<AuthResponse | null> {
  if (!refreshInFlight) {
    refreshInFlight = (async () => {
      // The double-submit cookie is set with every session and cleared with it: without it
      // there is no session to restore and the call could only fail (403 csrfFailed).
      if (!readCookie(CSRF_COOKIE)) {
        accessToken = null
        return null
      }
      try {
        const res = await send('POST', '/auth/refresh', { auth: false, cookie: true })
        if (!res.ok) {
          // A server that is restarting, upgrading (503) or throttling (429) has not ended the
          // session: keep it and surface the error, like a network failure, so the user is not
          // signed out (and does not lose unsaved text) over a transient answer.
          if (isTransient(res.status)) throw await toApiError(res)
          // The problem body is not needed; release the connection.
          void res.body?.cancel().catch(() => {})
          accessToken = null
          return null
        }
        const auth = (await res.json()) as AuthResponse
        accessToken = auth.accessToken
        listener.onRefreshed?.(auth)
        return auth
      } catch (err) {
        // A network failure or a transient server answer is not proof the session ended;
        // keep state and surface it.
        if (err instanceof ApiError && (err.status === 0 || isTransient(err.status))) throw err
        accessToken = null
        return null
      }
    })().finally(() => {
      refreshInFlight = null
    })
  }
  return refreshInFlight
}

/** Low-level call returning the Response; retries once after a refresh on 401. */
export async function rawRequest(method: string, path: string, opts: RequestOptions = {}): Promise<Response> {
  const hadToken = opts.auth !== false && accessToken !== null
  let res = await send(method, path, opts)
  if (res.status === 401 && hadToken) {
    const auth = await refreshSession()
    if (!auth) {
      listener.onSessionLost?.()
      throw await toApiError(res)
    }
    if (opts.signal?.aborted) throw new DOMException('Aborted', 'AbortError')
    res = await send(method, path, opts)
    if (res.status === 401) listener.onSessionLost?.()
  }
  if (!res.ok) {
    const err = await toApiError(res)
    if (err.status === 403 && err.is('mfaEnrolmentRequired')) listener.onMfaEnrolmentRequired?.()
    if (err.actions.length) problemHook?.(err)
    throw err
  }
  return res
}

/** JSON request. Returns undefined for 204 responses. */
export async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const res = await rawRequest(method, path, opts)
  if (res.status === 204) return undefined as T
  const type = res.headers.get('content-type') ?? ''
  if (!type.includes('json')) return undefined as T
  return (await res.json()) as T
}

export const api = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, { ...opts, body }),
  put: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, { ...opts, body }),
  patch: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PATCH', path, { ...opts, body }),
  delete: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('DELETE', path, { ...opts, body }),
}

/** Encode a path segment (ticket number, IDs). */
export function seg(value: string): string {
  return encodeURIComponent(value)
}

/** Turn an absolute API path from a response (e.g. `downloadUrl`) into a path relative to BASE. */
export function apiPath(url: string): string {
  return url.startsWith(`${BASE}/`) ? url.slice(BASE.length) : url
}

export interface UploadOptions {
  signal?: AbortSignal
  onProgress?: (fraction: number) => void
}

function xhrUpload(path: string, form: FormData, opts: UploadOptions): Promise<{ status: number; text: string; type: string; rid: string | null }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', buildUrl(path))
    xhr.setRequestHeader('Accept', 'application/json')
    if (accessToken) xhr.setRequestHeader('Authorization', `Bearer ${accessToken}`)
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && e.total > 0) opts.onProgress?.(e.loaded / e.total)
    }
    xhr.onload = () =>
      resolve({
        status: xhr.status,
        text: xhr.responseText,
        type: xhr.getResponseHeader('content-type') ?? '',
        rid: xhr.getResponseHeader('x-request-id'),
      })
    xhr.onerror = () => reject(new ApiError(0, { title: 'Network error', detail: 'The server could not be reached.' }))
    xhr.onabort = () => reject(new DOMException('Aborted', 'AbortError'))
    if (opts.signal) {
      if (opts.signal.aborted) {
        reject(new DOMException('Aborted', 'AbortError'))
        return
      }
      opts.signal.addEventListener('abort', () => xhr.abort(), { once: true })
    }
    xhr.send(form)
  })
}

/**
 * Multipart upload with progress (fetch cannot report upload progress). Uses the in-memory
 * access token and retries once after a refresh on 401, like `rawRequest`.
 */
export async function upload<T>(path: string, form: FormData, opts: UploadOptions = {}): Promise<T> {
  const hadToken = accessToken !== null
  let res = await xhrUpload(path, form, opts)
  if (res.status === 401 && hadToken) {
    const auth = await refreshSession()
    if (!auth) {
      listener.onSessionLost?.()
    } else {
      opts.onProgress?.(0)
      res = await xhrUpload(path, form, opts)
      if (res.status === 401) listener.onSessionLost?.()
    }
  }
  let body: unknown = undefined
  if (res.type.includes('json') && res.text) {
    try {
      body = JSON.parse(res.text)
    } catch {
      // fall through with no body
    }
  }
  if (res.status < 200 || res.status >= 300) {
    const problem = (body ?? {}) as ProblemDetails
    if (!problem.requestId && res.rid) problem.requestId = res.rid
    throw new ApiError(res.status, problem)
  }
  return body as T
}
