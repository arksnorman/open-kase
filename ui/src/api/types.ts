// API DTOs. The OpenAPI 3.1 document (docs/openapi.json) is authoritative; `npm run gen:api`
// regenerates `schema.d.ts` from it. Types whose generated schema has the same shape are plain
// aliases. The rest stay hand-written because the document types some fields as plain strings
// (e.g. `role`, `status`, `theme`) where the contract fixes the values; those are narrowed here and
// `SchemaConformance` at the bottom fails the type check if one drifts from the generated schema
// (no longer assignable, or a field the API does not send).

import type { components } from './schema'

type S = components['schemas']

export type ThemeChoice = 'system' | 'light' | 'dark'

export interface User {
  id: string
  email: string
  /** Sign-in name, an alternative to the email address. */
  username: string
  /** Display name: the name parts joined with single spaces (read-only). */
  name: string
  givenName: string
  middleName: string
  /** Empty only for a user migrated with a one-word name, until next edited. */
  familyName: string
  isAdmin: boolean
  mfaEnabled: boolean
  theme: ThemeChoice
  /** Markdown reply signature the server appends to your public replies (may be empty). */
  signature: string
  /** true = sent. Notices about tickets assigned to you cannot be muted. */
  emailPreferences: EmailPreferences
  /** If-Match for `PATCH /me` (display name). */
  version: number
}

export type EmailPreferences = S['EmailPrefs']

/** The two user roles. */
export type Role = 'admin' | 'agent'

export type GroupRef = S['Ref']

export interface AuthResponse {
  accessToken: string
  expiresIn: number
  /** Administrator without MFA while `auth.requireAdminMfa` is on: only enrolment routes work. */
  mfaEnrolmentRequired: boolean
  user: User
}

/**
 * `POST /auth/login`: either a full session (`accessToken`, `user`) or, for a user with TOTP
 * MFA, `mfaRequired: true` + a single-use `mfaToken` for `POST /auth/mfa` (no session yet).
 */
export type LoginResult = S['Login']
export type MfaStatus = S['MfaStatus']
export type TotpEnrolment = S['TotpEnrolment']
export type RecoveryCodes = S['RecoveryCodes']
/** Step-up for turning MFA off or replacing recovery codes: a TOTP code or an unused recovery code. */
export type MfaStepUp = { code?: string; recoveryCode?: string }
export type Invitation = S['Invitation']
/** Second factor for `POST /auth/mfa`: a 6-digit TOTP code or a recovery code. */
export type SecondFactor = { code: string } | { recoveryCode: string }

export interface ItemsResponse<T> {
  items: T[]
}

export interface Page<T> {
  items: T[]
  nextCursor: string | null
}

export interface UserRef {
  id: string
  name: string
}

export type ContactRef = S['ContactRef']
/** The organisation embedded in a ticket (set explicitly by the creator; never derived from the requester). */
export type OrganisationRef = S['OrganisationRef']
/** A customer organisation an integrating system serves. */
export type Organisation = S['Organisation']
export interface OrganisationInput {
  name?: string
  /** Empty clears. */
  externalRef?: string
  /** PATCH only: false deactivates, true reactivates. */
  active?: boolean
}


export type Priority = 1 | 2 | 3 | 4

export type StatusCategory =
  | 'open'
  | 'waitingCustomer'
  | 'onHold'
  | 'resolved'
  | 'archived'

export type TicketStatus = S['StatusRef']

export interface Ticket {
  id: string
  /** Shown as #1042 (ticketRef). */
  number: number
  projectId: string
  subject: string
  requester: ContactRef
  /** Set explicitly by the creator or staff; null when none. */
  organisation: OrganisationRef | null
  /** Addresses copied on every email to the requester (lower case, at most 10). */
  ccEmails: string[]
  group: GroupRef
  source: 'web' | 'api'
  assignee: UserRef | null
  priority: Priority
  requestedPriority: Priority | null
  status: TicketStatus
  requestType: { id: string; name: string }
  watching: boolean
  watcherCount: number
  /** Send as `expectedLastPublicId` when posting so a newer reply is never overlooked. */
  lastPublicMessageId: string | null
  createdAt: string
  updatedAt: string
  activityAt: string
  resolvedAt: string | null
  archivedAt: string | null
  version: number
  /** Normalized, sorted. Staff working data (empty for integrations). */
  tags: string[]
  /** First public reply by staff. */
  firstResponseAt: string | null
  /** null: SLA off, no cycle, or no target for the priority. */
  sla: TicketSLA | null
  /** Staff only: set while the ticket is On hold; null otherwise. */
  hold: S['Hold'] | null
}

// ---- Milestone 6: SLA, tags, canned replies, reports. ----

/** SLA is first response only. */
export type SLAKind = 'firstResponse'
export type SLAState = 'ok' | 'dueSoon' | 'breached' | 'met'

export interface SLATarget {
  kind: SLAKind
  state: 'running' | 'met' | 'stopped'
  budgetSeconds: number | null
  elapsedSeconds: number
  dueAt: string | null
  warnAt: string | null
  metAt: string | null
  breachedAt: string | null
}

/** `state` is evaluated by the server at read time. */
export interface TicketSLA {
  kind: SLAKind
  state: SLAState
  /** Running target only. */
  dueAt: string | null
  /** A deadline of this cycle was missed (never erased). */
  breached: boolean
  cycle: number
  /** The SLA version the cycle started with. */
  version: number
  targets: SLATarget[]
}

export interface SLACycle {
  cycle: number
  current: boolean
  priority: number
  version: number
  startedAt: string
  closedAt: string | null
  /** Null while the cycle is open. */
  closedReason: 'met' | 'resolved' | 'archived' | 'slaOff' | null
  breaches: { kind: SLAKind; at: string }[]
  targets: SLATarget[]
}

export interface TicketSLADetail {
  current: TicketSLA | null
  cycles: SLACycle[]
}

export type SLAPriorityTargets = S['Targets']

/** First-response targets per priority, wall-clock minutes 24/7. */
export interface SLAInput {
  active: boolean
  /** 50–95. */
  warningPercent: number
  targets: Record<'1' | '2' | '3' | '4', SLAPriorityTargets>
}

export interface SLASettings extends SLAInput {
  /** Null until first saved. */
  updatedAt: string | null
  /** 0 until first saved; sent as If-Match. */
  version: number
}

export type TagCount = S['Tag']

/** A reason a ticket can be put on hold for; a hold lasts exactly `maxDays`. */
export type HoldReason = S['HoldReason']
export interface HoldReasonInput {
  name?: string
  maxDays?: number
}

export interface Macro {
  id: string
  projectId: string
  name: string
  body: string
  visibility: MessageVisibility
  active: boolean
  createdAt: string
  updatedAt: string
  version: number
}

export interface MacroInput {
  name?: string
  body?: string
  visibility?: MessageVisibility
  active?: boolean
}

export interface MacroPreview {
  macroId: string
  /** Rendered (variables substituted, Markdown-escaped values). */
  body: string
  visibility: MessageVisibility
}

export interface ReportSummary {
  kind: 'boundedLiveTraversal'
  generatedAt: string
  range: { from: string; to: string; timezone: 'UTC' }
  bounds: { maxRangeDays: number; maxSamples: number; note: string }
  backlog: {
    total: number
    byStatus: { id: string; name: string; category: StatusCategory; count: number }[]
    byPriority: { priority: Priority; count: number }[]
    byGroup: { id: string; name: string; count: number }[]
  }
  createdVsResolved: { date: string; created: number; resolved: number }[]
  firstResponse: { medianSeconds: number | null; sampleSize: number; truncated: boolean }
  resolution: { medianSeconds: number | null; sampleSize: number; truncated: boolean }
  /** First-response breaches recorded in the range. */
  slaBreaches: { firstResponse: number }
  reopen: { resolved: number; reopened: number; rate: number | null }
}

export type MessageVisibility = 'public' | 'agent'

export interface Attachment {
  id: string
  filename: string
  size: number
  contentType: string
  scanStatus: 'unscanned'
  /** Absolute API path, e.g. "/api/v1/projects/{id}/attachments/{id}/download". Needs the bearer token. */
  downloadUrl: string
  /** Only on a staged (not yet attached) upload. */
  expiresAt?: string
  /** Never set on a stored file (see RemovedAttachment). */
  removed?: false
}

/**
 * A message's file that an administrator removed with a redaction (`removeAttachments`): only the
 * id is left, no name, size or type; its download answers `410 attachmentRemoved`.
 */
export interface RemovedAttachment {
  id: string
  removed: true
}

/** An entry of a message's `attachments` (every file it was sent with). */
export type MessageAttachment = Attachment | RemovedAttachment

/** `staff`: a user; `bot`: an agent token acting as support staff; `contact`: the requester. */
export type AuthorContext = 'staff' | 'bot' | 'contact'

export interface Message {
  id: string
  ticketId: string
  visibility: MessageVisibility
  author: { id: string; name: string; context: AuthorContext }
  body: string
  attachments: MessageAttachment[]
  createdAt: string
  /** Set when an administrator redacted the body (it is then replaced server-side). */
  redactedAt: string | null
}

/** `POST .../messages` response: the message plus the updated ticket (new `version`). */
export type PostedMessage = Message & { ticket: Ticket }

export type ScheduledState = 'scheduled' | 'needsReview' | 'sent' | 'cancelled'

/**
 * A reply or note waiting to be sent by the server at `sendAt` (private to its author; project
 * admins also list it). `needsReview`: not sent automatically, e.g. the requester replied after
 * it was scheduled (`reason`).
 */
export interface ScheduledMessage {
  id: string
  ticketId: string
  visibility: MessageVisibility
  author: { id: string; name: string; context: AuthorContext }
  body: string
  /** A sent one lists files a later redaction removed as placeholders. */
  attachments: MessageAttachment[]
  thenStatusId: string | null
  /** false: the signatures are skipped when it is sent. */
  addSignature: boolean
  sendAt: string
  state: ScheduledState
  reason: string | null
  baseLastPublicId: string | null
  sentMessageId: string | null
  sentAt: string | null
  createdAt: string
  updatedAt: string
  version: number
}

export type ScheduleMessageInput = S['ScheduleMessageRequest']

/** Server-side draft per (ticket, author, audience); `version: 0` means nothing is stored. */
export type Draft = S['Draft']

export type QueueView =
  | 'myWork'
  | 'unassigned'
  | 'watching'
  | 'awaitingCustomer'
  | 'onHold'
  | 'dueSoon'
  | 'resolved'
  | 'archived'
  | 'all'

export type QueueCounts = S['QueueCounts']

// ---- Admin resources (shapes fixed by the implementation, docs/api-contract-v1.md). ----

export interface Project {
  id: string
  /** The configured product name (settings product.name). */
  name: string
  defaultGroupId: string
  /** A workspace reply signature is configured (text not exposed). */
  hasSignature: boolean
  /** Send the requester a "we received your request" email for new tickets. */
  requesterAcknowledgement: boolean
  createdAt: string
  updatedAt: string
  version: number
}

export type Group = S['Group']

export interface Member {
  user: { id: string; name: string; email: string; username: string }
  userId: string
  role: Role
  groups: GroupRef[]
  /** The user account is active. */
  active: boolean
  createdAt: string
}

/** A user's name parts (the display name is derived from them). */
export interface NameParts {
  givenName: string
  middleName: string
  familyName: string
}

export interface UserSummary {
  id: string
  email: string
  username: string
  name: string
  givenName: string
  middleName: string
  familyName: string
  isAdmin: boolean
  mfaEnabled: boolean
  status: 'active' | 'disabled' | 'invited'
  createdAt: string
  updatedAt: string
  version: number
}

/** An API token scope with its description (`GET /integration-scopes`); one list for every token. */
export type IntegrationScope = S['Scope']

export interface Integration {
  id: string
  projectId: string
  name: string
  scopes: string[]
  status: 'active' | 'revoked'
  tokenHint: string
  createdAt: string
  lastUsedAt: string | null
  revokedAt: string | null
  /** If-Match for the rename PATCH. */
  version: number
}

/** Create response; `token` is shown exactly once. */
export interface IntegrationCreated extends Integration {
  token: string
}

export interface WorkflowStatus {
  id: string
  name: string
  category: StatusCategory
  builtin: boolean
}

export type RequestType = S['RequestType']

export type RequesterInput = { name: string; email: string; externalId?: string } | { contactId: string }

export interface CreateTicketInput {
  subject: string
  body: string
  requester: RequesterInput
  priority?: Priority
  requestTypeId?: string
  groupId?: string
  attachmentIds?: string[]
  organisationId?: string
  ccEmails?: string[]
}

/** Staff edits. The subject is locked after creation (only the creating integration may change it). */
export interface TicketPatch {
  priority?: Priority
  requestTypeId?: string
  /** Replaces the set. */
  tags?: string[]
  /** Empty clears. */
  organisationId?: string
  /** Replaces the CC list; empty clears. */
  ccEmails?: string[]
}

/** A staff user watching a ticket who can currently read it. */
export interface Watcher {
  id: string
  name: string
  username?: string
}

export type CreateMessageInput = S['PostMessageRequest']

export type TransitionAction = 'resolve' | 'reopen' | 'archive' | 'hold' | 'setStatus'

export interface TransitionInput {
  action: TransitionAction
  statusId?: string
  reason?: string
  /** Required with `hold`. */
  holdReasonId?: string
  /** Only with `hold`: an optional internal note. */
  note?: string
}

// ---- Milestone 5: settings, deliveries, webhooks. ----

export type SmtpTlsMode = 'starttls' | 'tls' | 'none'

export interface SmtpSettings {
  enabled: boolean
  host: string
  port: number
  username: string
  /** Read-only; the password itself is never returned. */
  passwordConfigured: boolean
  fromAddress: string
  fromName: string
  tlsMode: SmtpTlsMode
  /** PEM bundle trusted in addition to the system roots; empty when none. */
  caCertificate: string
  tlsSkipVerify: boolean
}

/**
 * `GET /settings` (redacted). `PUT /settings` takes the full document without `configVersion`
 * and `smtp.passwordConfigured`; sections this UI does not edit (`auth`, `files`, and any later
 * additions) are sent back unchanged. The generated schema now declares `auth` and `files`.
 */
export type Settings = Omit<S['Settings'], 'smtp'> & { smtp: SmtpSettings; [section: string]: unknown }

export type DeliveryChannel = 'email' | 'webhook'

export type DeliveryStatus =
  | 'pending'
  | 'retrying'
  | 'sent'
  | 'failed'
  | 'unknownOutcome'
  | 'skipped'
  | 'skippedUnconfigured'

export interface Delivery {
  id: string
  channel: DeliveryChannel
  eventId: string | null
  eventType: string
  projectId: string | null
  ticketId: string | null
  ticketNumber: number | null
  endpointId: string | null
  recipient: {
    kind: 'user' | 'contact' | 'address' | 'endpoint'
    id: string | null
    address: string | null
    /** Current name and email of a user or contact recipient; the endpoint name (and url) for webhooks (looked up when listed). */
    name: string | null
    email: string | null
    url: string | null
  }
  reason: 'requester' | 'cc' | 'assignee' | 'watcher' | 'groupQueue' | 'author' | 'endpoint' | 'test'
  template: string | null
  status: DeliveryStatus
  attempts: number
  nextAttemptAt: string | null
  lastAttemptAt: string | null
  errorClass: string | null
  error: string | null
  statusCode: number | null
  responseSnippet: string | null
  responseMs: number | null
  messageId: string | null
  createdAt: string
  updatedAt: string
  completedAt: string | null
}

/** One audit log entry (GET /audit-events, admins). */
export type AuditEvent = S['AuditEvent']
export type AuditActorType = S['AuditActor']['type']
/** One entry of the audit action catalog (GET /audit-actions). */
export type AuditAction = S['AuditAction']
/** One entry of the audit actor catalog (GET /audit-actors). */
export type AuditActorOption = S['AuditActorOption']
export type AuditActorList = S['AuditActorList']

export interface AuditFilters {
  actorType?: AuditActorType | ''
  /** User or token ID, or a system actor kind (with actorType). */
  actorId?: string
  /** Exact action code from the catalog. */
  action?: string
  ticket?: string
  from?: string
  to?: string
}

export interface DeliveryFilters {
  channel?: DeliveryChannel | ''
  status?: DeliveryStatus | ''
  eventId?: string
}

export type Webhook = S['Webhook']

export type WebhookAuthType = 'none' | 'basic' | 'token'

export type WebhookEventType = S['WebhookEventType']

/** Header values, password, token and client key are write-only: omitted or empty keeps the stored value. */
export interface WebhookInput {
  name?: string
  url?: string
  eventTypes?: string[]
  active?: boolean
  includeAgentNotes?: boolean
  headers?: { name: string; value?: string }[]
  auth?: { type: WebhookAuthType; username?: string; password?: string; token?: string }
  tls?: { caCertificate?: string; clientCertificate?: string; clientKey?: string }
}

// ---- Milestone 7: full-text search (aliases of the generated schemas). ----

export type SearchPage = S['SearchPage']
export type SearchHit = S['SearchHit']
export type Snippet = S['Snippet']
export type Highlight = S['Highlight']

// ---- Drift guard: hand-narrowed types must stay compatible with the generated schemas. ----

type Assert<T extends true> = T
// ---- Ticket workflow: links, follow-ups, activity, automation, live updates. ----

export type LinkType = 'relatedTo' | 'duplicateOf' | 'followUpTo' | 'copiedFrom'
/** Relation of the ticket in the path to the other ticket (incoming side reads `duplicatedBy` / `followedUpBy` / `copiedTo`). */
export type LinkRelation = LinkType | 'duplicatedBy' | 'followedUpBy' | 'copiedTo'

export interface TicketLink {
  id: string
  type: LinkType
  relation: LinkRelation
  direction: 'outgoing' | 'incoming'
  /** The other ticket, in the caller's projection (links to tickets the caller cannot read are omitted). */
  ticket: Ticket
  createdAt: string
}

/** `POST .../copy`: every field overrides the value copied from the original. */
export interface CopyTicketInput {
  subject?: string
  groupId?: string
  priority?: Priority
  requestTypeId?: string
  tags?: string[]
  body?: string
  includeAttachments?: boolean
}

export interface FollowUpInput {
  subject?: string
  body?: string
  priority?: Priority
}

/** One staff-safe audit event of a ticket (`GET .../activity`): field names only, never bodies or values. */
export interface ActivityItem {
  id: string
  action: string
  /** `service`: an API token (its name); `scopes`: the token scopes that allowed the action. */
  actor: { type: 'user' | 'service' | 'operator' | 'system'; id: string | null; name: string; scopes: string[] }
  target: string
  changes: string[]
  reason: string | null
  outcome: 'success' | 'failure'
  createdAt: string
}

export interface Automation {
  /** 0 = disabled. */
  archiveAfterResolvedDays: number
  /** 0 = disabled. */
  customerFollowupAfterHours: number
  maxReminders: number
  reminderCooldownHours: number
  /** 0 = disabled; requires reminders. */
  autoResolveAfterHours: number
  /** Any rule on (derived). */
  enabled: boolean
  /** 0 until first saved; sent as If-Match. */
  version: number
  updatedAt: string | null
}

export type AutomationInput = Pick<
  Automation,
  'archiveAfterResolvedDays' | 'customerFollowupAfterHours' | 'maxReminders' | 'reminderCooldownHours' | 'autoResolveAfterHours'
>

/** Outbox event types delivered by `GET /projects/{p}/events` as `invalidate`. */
export interface LiveInvalidate {
  type: string
  ticketId: string
  /** 0 for integrations. */
  version: number
}

/** `H` is assignable to the generated `G` and declares no field the API does not have. */
type Conforms<H extends G, G> = [Exclude<keyof H, keyof G>] extends [never] ? true : false

export type SchemaConformance = [
  Assert<Conforms<User, S['User']>>,
  Assert<Conforms<AuthResponse, S['Auth']>>,
  Assert<Conforms<UserRef, S['Ref']>>,
  Assert<Conforms<Ticket, S['Ticket']>>,
  Assert<Conforms<ScheduledMessage, S['ScheduledMessage']>>,
  Assert<Conforms<SLATarget, S['SLAClock']>>,
  Assert<Conforms<TicketSLA, S['SLA']>>,
  Assert<Conforms<SLACycle, S['SLACycle']>>,
  Assert<Conforms<TicketSLADetail, S['TicketSLA']>>,
  Assert<Conforms<SLASettings, S['SLASettings']>>,
  Assert<Conforms<SLAInput, S['PutSLARequest']>>,
  Assert<Conforms<Macro, S['Macro']>>,
  Assert<Conforms<HoldReasonInput, S['HoldReasonRequest']>>,
  Assert<Conforms<MacroInput, S['MacroRequest']>>,
  Assert<Conforms<MacroPreview, S['MacroPreview']>>,
  Assert<Conforms<ReportSummary, S['ReportSummary']>>,
  Assert<Conforms<Attachment, S['Attachment']>>,
  Assert<Conforms<RemovedAttachment, S['Attachment']>>,
  Assert<Conforms<Message, S['Message']>>,
  Assert<Conforms<PostedMessage, S['PostedMessage']>>,
  Assert<Conforms<Project, S['Project']>>,
  Assert<Conforms<Member, S['Member']>>,
  Assert<Conforms<UserSummary, S['UserAdmin']>>,
  Assert<Conforms<Integration, S['Integration']>>,
  Assert<Conforms<IntegrationCreated, S['IntegrationCreated']>>,
  Assert<Conforms<WorkflowStatus, S['Status']>>,
  Assert<Conforms<CreateTicketInput, S['CreateTicketRequest']>>,
  Assert<Conforms<TicketPatch, S['PatchTicketRequest']>>,
  Assert<Conforms<Watcher, S['Watcher']>>,
  Assert<Conforms<TransitionInput, S['TransitionTicketRequest']>>,
  Assert<Conforms<SmtpSettings, S['SettingsSMTP']>>,
  Assert<Pick<Settings, keyof S['Settings']> extends S['Settings'] ? true : false>,
  Assert<Conforms<Delivery, S['Delivery']>>,
  Assert<Conforms<WebhookInput, S['WebhookRequest']>>,
  Assert<Conforms<{ delivery: Delivery }, S['DeliveryEnvelope']>>,
  Assert<Conforms<TicketLink, S['Link']>>,
  Assert<Conforms<ActivityItem, S['Activity']>>,
  Assert<Conforms<Automation, S['Automation']>>,
  Assert<Conforms<AutomationInput, S['PutAutomationRequest']>>,
]
