// Plain-language names for webhook event types and small summaries used by the webhook screens.
import type { Webhook } from '@/api/types'

const EVENTS: Record<string, { name: string; short: string }> = {
  'ticket.created': { name: 'Ticket created', short: 'Created' },
  'ticket.updated': { name: 'Ticket changed', short: 'Changed' },
  'ticket.message.public': { name: 'Public reply', short: 'Public reply' },
  'ticket.message.agentNote': { name: 'Agent note', short: 'Agent note' },
  'ticket.assigned': { name: 'Assigned', short: 'Assigned' },
  'ticket.statusChanged': { name: 'Status changed', short: 'Status' },
  'ticket.slaWarning': { name: 'SLA due soon', short: 'SLA due soon' },
  'ticket.slaBreached': { name: 'SLA breached', short: 'SLA breached' },
  'ticket.followupReminder': { name: 'Follow-up reminder', short: 'Reminder' },
  'webhook.test': { name: 'Test', short: 'Test' },
}

/** "Ticket created" for ticket.created; the code itself for an unknown type. */
export function eventName(type: string): string {
  return EVENTS[type]?.name ?? type
}

/** Short form for chips ("Created", "Status"). */
export function eventShort(type: string): string {
  return EVENTS[type]?.short ?? type
}

/** "No auth", "Basic auth" or "Token auth", plus header count and custom CA. */
export function authSummary(w: Webhook): string {
  const parts = [w.auth.type === 'basic' ? 'Basic auth' : w.auth.type === 'token' ? 'Token auth' : 'No auth']
  if (w.headers.length) parts.push(`${w.headers.length} header${w.headers.length === 1 ? '' : 's'}`)
  if (w.tls.caCertificate) parts.push('Custom CA')
  return parts.join(' · ')
}

export type StripKind = 'delivered' | 'retrying' | 'failed'

/** Health-strip kind of a delivery status. */
export function stripKind(status: string): StripKind {
  return status === 'sent' ? 'delivered' : status === 'failed' ? 'failed' : 'retrying'
}

export interface Health {
  /** Oldest first, at most ten. */
  bars: StripKind[]
  failed: number
  failing: boolean
  summary: string
}

/** The strip and its accessible summary from a webhook's stats. */
export function health(w: Webhook): Health {
  const bars = [...w.stats.lastDeliveries].reverse().map((d) => stripKind(d.status))
  const count = (k: StripKind) => bars.filter((b) => b === k).length
  const failed = count('failed')
  // Failing: at least half of a meaningful sample failed.
  const failing = bars.length >= 3 ? failed * 2 >= bars.length : bars.length > 0 && failed === bars.length
  const summary = bars.length
    ? `Last ${bars.length} deliveries: ${count('delivered')} delivered, ${count('retrying')} retrying, ${failed} failed`
    : 'No deliveries in the last 24 hours'
  return { bars, failed, failing, summary }
}

/** 0.983 -> "98%"; a dash when there is no data. */
export function percent(rate: number | null): string {
  return rate === null ? '—' : `${Math.round(rate * 100)}%`
}
