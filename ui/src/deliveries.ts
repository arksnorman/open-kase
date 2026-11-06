// Labels and explanations for notification delivery records (docs/api-contract-v1.md, Milestone 5).
import type { Delivery, DeliveryStatus } from '@/api/types'
import type { AvatarRole } from '@/components/base/BaseAvatar.vue'

export const DELIVERY_STATUSES: DeliveryStatus[] = [
  'pending',
  'retrying',
  'sent',
  'failed',
  'unknownOutcome',
  'skipped',
  'skippedUnconfigured',
]

const LABELS: Record<DeliveryStatus, string> = {
  pending: 'Pending',
  retrying: 'Retrying',
  sent: 'Sent',
  failed: 'Failed',
  unknownOutcome: 'Unknown outcome',
  skipped: 'Skipped',
  skippedUnconfigured: 'Not configured',
}

const EXPLANATIONS: Record<DeliveryStatus, string> = {
  pending: 'Queued; a background worker sends it shortly.',
  retrying: 'A temporary error occurred; it is retried automatically (1 min, 5 min, 30 min, 2 h, 12 h).',
  sent: 'Accepted by the mail server or answered with a 2xx status by the endpoint.',
  failed: 'Dead-lettered: retries were exhausted or the error was permanent. It can be retried manually.',
  unknownOutcome:
    'The connection broke after the message was handed to the mail server, so it may have been delivered. It is not retried automatically; retrying may send a duplicate.',
  skipped: 'Not sent on purpose; the reason is shown as the error class.',
  skippedUnconfigured: 'Email is not enabled or configured (in Settings, Configuration, Email section). This is not an error.',
}

export function deliveryStatusLabel(s: DeliveryStatus): string {
  return LABELS[s] ?? s
}

export function deliveryStatusExplanation(s: DeliveryStatus): string {
  return EXPLANATIONS[s] ?? ''
}

export function deliveryStatusTone(s: DeliveryStatus): 'neutral' | 'note' | 'success' | 'warn' | 'danger' {
  switch (s) {
    case 'sent':
      return 'success'
    case 'failed':
      return 'danger'
    case 'unknownOutcome':
    case 'retrying':
      return 'warn'
    case 'skippedUnconfigured':
      return 'note'
    default:
      return 'neutral'
  }
}

/** States that no longer change without a manual retry. */
export function isTerminal(s: DeliveryStatus): boolean {
  return s !== 'pending' && s !== 'retrying'
}

export function isRetryable(d: Delivery): boolean {
  return d.status === 'failed' || d.status === 'unknownOutcome'
}

const ERROR_CLASSES: Record<string, string> = {
  recipientNoAccess: 'The recipient no longer has access to the ticket.',
  recipientInactive: 'The recipient account is not active.',
  mutedByPreference: 'The recipient muted this kind of email.',
  noLongerRequester: 'The contact is no longer the ticket requester.',
  requesterEmailsOff: 'Requester emails are turned off (Settings, Configuration, Email). Use webhooks to notify requesters.',
  noLongerCc: 'The address was removed from the ticket’s CC list before sending.',
  ticketUnavailable: 'The ticket was deleted or is unavailable.',
  baseUrlNotConfigured: 'No base URL is set (Settings, Configuration, General), so links cannot be built.',
  smtpNotConfigured: 'Email is not enabled or configured.',
  endpointDeleted: 'The webhook endpoint was deleted.',
  endpointInactive: 'The webhook is paused.',
  eventTypeUnsubscribed: 'The endpoint no longer subscribes to this event type.',
  attemptsExhausted: 'All automatic attempts failed.',
  network: 'The destination could not be reached (DNS, connection or TLS error).',
}

export function errorClassExplanation(c: string | null): string | undefined {
  return c ? ERROR_CLASSES[c] : undefined
}

/** Avatar kind of a recipient: staff user, requester contact, webhook endpoint, or a plain (CC) address. */
export function deliveryAvatar(d: Delivery): AvatarRole {
  const r = d.recipient
  if (r.kind === 'endpoint' || d.channel === 'webhook') return 'webhook'
  if (r.kind === 'user') return 'staff'
  if (r.kind === 'contact') return 'requester'
  return 'email'
}

/** Human-readable recipient: "Name <email>" for users and contacts, the endpoint name (else URL) for webhooks. */
export function deliveryRecipient(d: Delivery): string {
  const r = d.recipient
  if (r.kind === 'endpoint' || d.channel === 'webhook') return r.name ?? r.url ?? r.address ?? 'Deleted endpoint'
  const email = r.email ?? r.address
  if (r.name) return email ? `${r.name} <${email}>` : r.name
  if (email) return email
  return r.kind === 'user' ? 'Deleted user' : r.kind === 'contact' ? 'Deleted contact' : r.kind
}

/** Tooltip of the recipient: the webhook's URL under its name, else the same text. */
export function deliveryRecipientTitle(d: Delivery): string {
  const r = d.recipient
  return (r.kind === 'endpoint' || d.channel === 'webhook') && r.name && r.url ? `${r.name}\n${r.url}` : deliveryRecipient(d)
}
