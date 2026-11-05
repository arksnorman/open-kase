// Problem-details error type shared by every API call.

export interface FieldError {
  field: string
  code: string
  message: string
}

/** Follow-up hint on a problem, e.g. `createFollowUp` on `ticketArchived`. Never a retry of the request. */
export interface ProblemAction {
  rel: string
  title: string
  method: string
  /** Absolute API path, e.g. `/api/v1/tickets/7/follow-up`. */
  href: string
}

export interface ProblemDetails {
  type?: string
  title?: string
  status?: number
  code?: string
  detail?: string
  requestId?: string
  errors?: FieldError[]
  actions?: ProblemAction[]
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly type: string | undefined
  readonly title: string
  readonly requestId: string | undefined
  readonly fieldErrors: FieldError[]
  readonly actions: ProblemAction[]
  /** Seconds from the response's Retry-After header (every 429 sends one), else null. */
  readonly retryAfter: number | null

  constructor(status: number, problem: ProblemDetails, retryAfter: number | null = null) {
    super(problem.detail || problem.title || fallbackMessage(status))
    this.name = 'ApiError'
    this.status = status
    this.code = problem.code ?? (status === 0 ? 'networkError' : `http${status}`)
    this.type = problem.type
    this.title = problem.title ?? (status === 0 ? 'Network error' : 'Request failed')
    this.requestId = problem.requestId
    this.fieldErrors = Array.isArray(problem.errors) ? problem.errors : []
    this.actions = Array.isArray(problem.actions) ? problem.actions : []
    this.retryAfter = retryAfter
  }

  /** Message for one field, if the server reported one. */
  field(name: string): string | undefined {
    return this.fieldErrors.find((e) => e.field === name)?.message
  }

  is(code: string): boolean {
    return this.code === code
  }

  /** The problem's action with this `rel`, if any. */
  action(rel: string): ProblemAction | undefined {
    return this.actions.find((a) => a.rel === rel)
  }
}

/** Text for a response without a problem document (typically from a proxy in front of the server). */
function fallbackMessage(status: number): string {
  if (status === 502 || status === 503 || status === 504) return `The server is not available right now (${status}). Try again in a moment.`
  if (status === 413) return 'The request is larger than the server accepts (413).'
  return `Request failed (${status})`
}

export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError'
}

/**
 * "Try again in 15 minutes." from a throttled answer's Retry-After, else `fallback`. The server's
 * own texts only say "later", and a sign-in lock lasts 15 minutes, not "a moment".
 */
export function retryHint(err: unknown, fallback: string): string {
  if (!(err instanceof ApiError) || err.status !== 429 || err.retryAfter === null) return fallback
  const s = Math.max(1, err.retryAfter)
  const [n, unit] = s < 60 ? [s, 'second'] : s < 5400 ? [Math.ceil(s / 60), 'minute'] : [Math.ceil(s / 3600), 'hour']
  return `Try again in ${n} ${unit}${n === 1 ? '' : 's'}.`
}

/** Human-readable text for any thrown value; used by views that show a single error line. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const ref = err.requestId ? ` (reference ${err.requestId})` : ''
    if (err.status === 429 && err.retryAfter !== null) {
      // Replace the server's "Try again later." / "Retry later." with the actual wait.
      const base = err.message.replace(/\s*(Try again|Retry) later\.?$/, '').replace(/([^.!?])$/, '$1.')
      return `${base} ${retryHint(err, '')}${ref}`
    }
    return `${err.message}${ref}`
  }
  if (err instanceof Error) return err.message
  return 'Something went wrong.'
}
