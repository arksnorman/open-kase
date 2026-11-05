import type { Priority, SLAState, StatusCategory, TicketSLA } from '@/api/types'

const PRIORITY_NAMES: Record<Priority, string> = { 1: 'Critical', 2: 'High', 3: 'Normal', 4: 'Low' }

/** A ticket's human reference: "#" and its number ("#1042"). Tickets have no key. */
export function ticketRef(t: { number: number }): string {
  return `#${t.number}`
}

/** Route location of a ticket page (the path carries the plain number). */
export function ticketRoute(t: { number: number }) {
  return { name: 'ticket' as const, params: { ticketNumber: String(t.number) } }
}

/** Label shown in badges, pickers and filters: "Critical", "High", "Normal" or "Low"
 * (owner decision 2026-10-31: names, no P-numbers; the API keeps the numbers 1–4). */
export function priorityShort(p: Priority): string {
  return PRIORITY_NAMES[p]
}

/** Spoken/tooltip label, e.g. "Critical priority". */
export function priorityLong(p: Priority): string {
  return `${PRIORITY_NAMES[p]} priority`
}

export const PRIORITIES: Priority[] = [1, 2, 3, 4]

export function statusToneClass(category: StatusCategory): string {
  return statusDotClass(category).replace('lg-dot-', 'lg-status-')
}

export function statusDotClass(category: StatusCategory): string {
  switch (category) {
    case 'open':
      return 'lg-dot-open'
    case 'waitingCustomer':
      return 'lg-dot-waiting'
    case 'onHold':
      return 'lg-dot-hold'
    case 'resolved':
      return 'lg-dot-resolved'
    case 'archived':
      return 'lg-dot-archived'
  }
}

export function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean)
  const first = parts[0]?.[0] ?? '?'
  const last = parts.length > 1 ? (parts[parts.length - 1]?.[0] ?? '') : ''
  return (first + last).toUpperCase()
}

/** Owner rule: dates show as "just now" / "12 min ago" / "3h ago" / "29d ago"; see RelativeTime.vue. */
export function timeAgo(iso: string, now = Date.now()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const diff = Math.round((now - t) / 1000)
  const future = diff < 0
  const abs = Math.abs(diff)
  let text: string
  if (abs < 45) return future ? 'in a moment' : 'just now'
  else if (abs < 3600) text = `${Math.max(1, Math.round(abs / 60))} min`
  else if (abs < 86400) text = `${Math.round(abs / 3600)}h`
  else if (abs < 86400 * 30) text = `${Math.round(abs / 86400)}d`
  else if (abs < 86400 * 365) text = `${Math.round(abs / (86400 * 30))}mo`
  else text = `${Math.round(abs / (86400 * 365))}y`
  return future ? `in ${text}` : `${text} ago`
}

/** Time left until `iso`, e.g. "3d 4h", "5h 20m", "40m"; "a moment" once it has passed (the server is about to end the hold). */
export function holdRemaining(iso: string, now = Date.now()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const mins = Math.ceil((t - now) / 60_000)
  if (mins <= 0) return 'a moment'
  const d = Math.floor(mins / 1440)
  const h = Math.floor((mins % 1440) / 60)
  const m = mins % 60
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`
  return `${m}m`
}

const fullFormat = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  timeZoneName: 'short',
})

/** Full local date-time with zone, e.g. "1 Oct 2026, 10:42 EAT" (browser locale). */
export function fullDateTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : fullFormat.format(d)
}

const dayFormat = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' })

/** Calendar day ("YYYY-MM-DD"), formatted without shifting it into another zone, e.g. "Thu, 1 Oct 2026". */
export function fullDay(day: string): string {
  const t = Date.parse(`${day.slice(0, 10)}T00:00:00Z`)
  return Number.isNaN(t) ? day : dayFormat.format(new Date(t))
}

/** Calendar-day distance from today (local), e.g. "today", "3d ago", "in 2mo". */
export function daysAgo(day: string, now = Date.now()): string {
  const [y, m, d] = day.slice(0, 10).split('-').map(Number)
  if (!y || !m || !d) return day
  const today = new Date(now)
  const diff = Math.round((Date.UTC(today.getFullYear(), today.getMonth(), today.getDate()) - Date.UTC(y, m - 1, d)) / 86400000)
  if (diff === 0) return 'today'
  if (diff === 1) return 'yesterday'
  if (diff === -1) return 'tomorrow'
  const abs = Math.abs(diff)
  const text = abs < 30 ? `${abs}d` : abs < 365 ? `${Math.round(abs / 30)}mo` : `${Math.round(abs / 365)}y`
  return diff < 0 ? `in ${text}` : `${text} ago`
}

export function fileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

export function roleLabel(role: string): string {
  switch (role) {
    case 'admin':
      return 'Admin'
    case 'agent':
      return 'Agent'
    default:
      return role
  }
}

/** Compact duration, e.g. "45 min", "4 h", "1 h 30 min", "2 d 3 h". */
export function duration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} s`
  const min = Math.round(s / 60)
  if (min < 60) return `${min} min`
  const h = Math.floor(min / 60)
  const m = min % 60
  if (h < 24) return m ? `${h} h ${m} min` : `${h} h`
  const d = Math.floor(h / 24)
  const rh = h % 24
  return rh ? `${d} d ${rh} h` : `${d} d`
}

/**
 * The two largest units of a duration, compact: "2h 15m", "3d 4h", "25m", "<1m" (SLA chips).
 */
export function shortDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return '<1m'
  const totalMin = Math.floor(s / 60)
  const d = Math.floor(totalMin / 1440)
  const h = Math.floor((totalMin % 1440) / 60)
  const m = totalMin % 60
  if (d) return h ? `${d}d ${h}h` : `${d}d`
  if (h) return m ? `${h}h ${m}m` : `${h}h`
  return `${m}m`
}

const SLA_KIND_LABELS: Record<string, string> = {
  firstResponse: 'First response',
}

export function slaKindLabel(kind: string): string {
  return SLA_KIND_LABELS[kind] ?? kind
}

const CATEGORY_LABELS: Record<StatusCategory, string> = {
  open: 'Open',
  waitingCustomer: 'Awaiting customer',
  onHold: 'On hold',
  resolved: 'Resolved',
  archived: 'Archived',
}

export function categoryLabel(c: string): string {
  return CATEGORY_LABELS[c as StatusCategory] ?? c
}

/**
 * The SLA state to show now: the server evaluates `state` at read time; this escalates it
 * locally as time passes `warnAt` / `dueAt`, so an open page does not show a stale state.
 */
export function slaDisplayState(s: TicketSLA | null | undefined, now = Date.now()): SLAState | null {
  if (!s) return null
  if ((s.state === 'ok' || s.state === 'dueSoon') && s.dueAt && Date.parse(s.dueAt) <= now) return 'breached'
  if (s.state === 'ok') {
    const warn = s.targets.find((t) => t.kind === s.kind && t.state === 'running')?.warnAt
    if (warn && Date.parse(warn) <= now) return 'dueSoon'
  }
  return s.state
}
