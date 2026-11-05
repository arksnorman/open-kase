// Typed endpoint functions for docs/api-contract-v1.md. Views and stores call these, never fetch.

import { api, apiPath, rawRequest, refreshSession, seg, setAccessToken, upload, type UploadOptions } from './client'
import type {
  MfaStepUp,
  Watcher,
  IntegrationScope,
  ActivityItem,
  Automation,
  AutomationInput,
  CopyTicketInput,
  FollowUpInput,
  LinkType,
  TicketLink,
  Attachment,
  AuthResponse,
  CreateMessageInput,
  CreateTicketInput,
  ScheduledMessage,
  ScheduleMessageInput,
  AuditAction,
  AuditActorList,
  AuditEvent,
  AuditFilters,
  Delivery,
  DeliveryFilters,
  EmailPreferences,
  Draft,
  HoldReason,
  HoldReasonInput,
  Organisation,
  OrganisationInput,
  Macro,
  MacroInput,
  MacroPreview,
  ReportSummary,
  SearchPage,
  SLAInput,
  SLASettings,
  TagCount,
  TicketSLADetail,
  Group,
  GroupRef,
  Integration,
  IntegrationCreated,
  Invitation,
  LoginResult,
  MfaStatus,
  RecoveryCodes,
  SecondFactor,
  TotpEnrolment,
  ItemsResponse,
  Member,
  NameParts,
  Message,
  MessageVisibility,
  Page,
  PostedMessage,
  Project,
  QueueCounts,
  QueueView,
  RequestType,
  Settings,
  ThemeChoice,
  Ticket,
  TicketPatch,
  TransitionInput,
  User,
  UserSummary,
  Webhook,
  WebhookEventType,
  WebhookInput,
  WorkflowStatus,
} from './types'

type Sig = { signal?: AbortSignal }
const t = (ticket: string) => `/tickets/${seg(ticket)}`

export const authApi = {
  /** Opens a session unless the user has MFA (`mfaRequired`); then call `mfa()` with the token. */
  /** `login` is an email address or a username (the server treats it as an email exactly when it contains "@"). */
  async login(login: string, password: string): Promise<LoginResult> {
    const res = await api.post<LoginResult>('/auth/login', { login, password }, { auth: false, cookie: true })
    if (res.accessToken) setAccessToken(res.accessToken)
    return res
  },
  async mfa(mfaToken: string, factor: SecondFactor): Promise<AuthResponse> {
    const res = await api.post<AuthResponse>('/auth/mfa', { mfaToken: mfaToken, ...factor }, { auth: false, cookie: true })
    setAccessToken(res.accessToken)
    return res
  },
  /** Always `202` for any address (no account enumeration). */
  requestPasswordReset: (email: string) =>
    api.post<{ status: string }>('/auth/password-reset/request', { email }, { auth: false }),
  completePasswordReset: (token: string, password: string) =>
    api.post<void>('/auth/password-reset/complete', { token, password }, { auth: false }),
  acceptInvitation: (input: { token: string; password: string; givenName?: string; middleName?: string; familyName?: string }) =>
    api.post<void>('/auth/invitations/accept', input, { auth: false }),
  refresh: refreshSession,
  async logout(): Promise<void> {
    try {
      await api.post<void>('/auth/logout', undefined, { cookie: true })
    } finally {
      setAccessToken(null)
    }
  },
}

export const meApi = {
  get: (o: Sig = {}) => api.get<User>('/me', o),
  /** Display name only; `409 versionConflict` when the account changed meanwhile. */
  updateProfile: (user: Pick<User, 'version'>, patch: Partial<NameParts>) => api.patch<User>('/me', patch, { ifMatch: user.version }),
  setTheme: (theme: ThemeChoice) => api.put<User>('/me/preferences', { theme }),
  setSignature: (signature: string) => api.put<User>('/me/preferences', { signature }),
  setEmailPreferences: (prefs: Partial<EmailPreferences>) =>
    api.put<User>('/me/preferences', { emailPreferences: prefs }),
  /** `204`; the user's other sessions are revoked, this one stays signed in. */
  changePassword: (currentPassword: string, newPassword: string) =>
    api.post<void>('/me/password', { currentPassword: currentPassword, newPassword: newPassword }),
  mfa: (o: Sig = {}) => api.get<MfaStatus>('/me/mfa', o),
  startTotp: (password: string) => api.post<TotpEnrolment>('/me/mfa/totp/enrol', { password }),
  /** Recovery codes are returned once; other sessions of the user are revoked. */
  confirmTotp: (code: string) => api.post<RecoveryCodes>('/me/mfa/totp/confirm', { code }),
  regenerateRecoveryCodes: (password: string, code: MfaStepUp) => api.post<RecoveryCodes>('/me/mfa/recovery-codes', { password, ...code }),
  disableMfa: (password: string, code: MfaStepUp) => api.post<void>('/me/mfa/disable', { password, ...code }),
}

export interface MemberQuery {
  q?: string
  groupId?: string
  userIds?: string[]
  /** Only active users. */
  active?: boolean
  cursor?: string | null
  limit?: number
}

export interface TicketListQuery {
  view: QueueView
  q?: string
  groupId?: string
  cursor?: string | null
  limit?: number
  /** Ad-hoc filters (any view; the server intersects them with the view), already comma-joined. */
  filters?: TicketFilterParams
}

/** List query parameters of the ad-hoc filters (comma-separated values). */
export interface TicketFilterParams {
  priority?: string
  tag?: string
  statusId?: string
  requestTypeId?: string
  organisationId?: string
  assignee?: string
  due?: string
}

const d = (ticket: string, visibility: MessageVisibility) => `${t(ticket)}/drafts/${visibility}`

export const ticketsApi = {
  list: (query: TicketListQuery, o: Sig = {}) =>
    api.get<Page<Ticket>>(`/tickets`, {
      ...o,
      query: {
        view: query.view,
        q: query.q,
        groupId: query.groupId,
        ...query.filters,
        cursor: query.cursor,
        limit: query.limit,
      },
    }),
  get: (ticket: string, o: Sig = {}) => api.get<Ticket>(t(ticket), o),
  create: (input: CreateTicketInput, idempotencyKey: string) =>
    api.post<Ticket>(`/tickets`, input, { idempotencyKey }),
  update: (ticket: Ticket, patch: TicketPatch) =>
    api.patch<Ticket>(t(ticket.id), patch, { ifMatch: ticket.version }),
  claim: (ticket: Ticket) =>
    api.post<Ticket>(`${t(ticket.id)}/claim`, undefined, { ifMatch: ticket.version }),
  release: (ticket: Ticket, reason: string) =>
    api.post<Ticket>(`${t(ticket.id)}/release`, { reason }, { ifMatch: ticket.version }),
  /**
   * `assigneeId` absent keeps the current assignee when still eligible in the target group;
   * `null` unassigns; an id assigns (must be an active member of the target group).
   */
  assign: (ticket: Ticket, input: { groupId: string; assigneeId?: string | null }) =>
    api.post<Ticket>(`${t(ticket.id)}/assignments`, input, { ifMatch: ticket.version }),
  transition: (ticket: Ticket, input: TransitionInput) =>
    api.post<Ticket>(`${t(ticket.id)}/transitions`, input, { ifMatch: ticket.version }),
  watchers: (ticket: string, o: Sig = {}) =>
    api.get<ItemsResponse<Watcher>>(`${t(ticket)}/watchers`, o),
  watch: (ticket: Ticket) => api.put<Ticket>(`${t(ticket.id)}/watchers/me`),
  unwatch: (ticket: Ticket) => api.delete<Ticket>(`${t(ticket.id)}/watchers/me`),
  messages: (ticket: string, cursor: string | null, o: Sig = {}) =>
    api.get<Page<Message>>(`${t(ticket)}/messages`, { ...o, query: { cursor, limit: 100 } }),
  postMessage: (ticket: string, input: CreateMessageInput, idempotencyKey: string) =>
    api.post<PostedMessage>(`${t(ticket)}/messages`, input, { idempotencyKey }),
  /** Active (scheduled / needs review) scheduled messages: mine, or everyone's for admins. */
  scheduled: (ticket: string, o: Sig = {}) =>
    api.get<ItemsResponse<ScheduledMessage>>(`${t(ticket)}/scheduled-messages`, o),
  schedule: (ticket: string, input: ScheduleMessageInput) =>
    api.post<ScheduledMessage>(`${t(ticket)}/scheduled-messages`, input),
  updateScheduled: (
    ticket: string,
    sm: ScheduledMessage,
    patch: { body?: string; sendAt?: string; thenStatusId?: string | null },
  ) => api.patch<ScheduledMessage>(`${t(ticket)}/scheduled-messages/${sm.id}`, patch, { ifMatch: sm.version }),
  cancelScheduled: (ticket: string, sm: ScheduledMessage) =>
    api.post<ScheduledMessage>(`${t(ticket)}/scheduled-messages/${sm.id}/cancel`, undefined, { ifMatch: sm.version }),
  sendScheduledNow: (ticket: string, sm: ScheduledMessage, expectedLastPublicId: string | null) =>
    api.post<PostedMessage>(
      `${t(ticket)}/scheduled-messages/${sm.id}/send`,
      expectedLastPublicId ? { expectedLastPublicId: expectedLastPublicId } : {},
      { ifMatch: sm.version },
    ),
  getDraft: (ticket: string, visibility: MessageVisibility, o: Sig = {}) =>
    api.get<Draft>(d(ticket, visibility), o),
  /** `version` 0 (nothing stored yet) sends no If-Match. */
  saveDraft: (
    ticket: string,
    visibility: MessageVisibility,
    input: { body: string; baseLastPublicId: string | null },
    version: number,
  ) => api.put<Draft>(d(ticket, visibility), input, version > 0 ? { ifMatch: version } : {}),
  deleteDraft: (ticket: string, visibility: MessageVisibility) =>
    api.delete<void>(d(ticket, visibility)),
  /** Stage a file (multipart field `file`); attach its id when sending within 24 h. */
  uploadAttachment: (file: File, opts: UploadOptions = {}) => {
    const form = new FormData()
    form.append('file', file, file.name)
    return upload<Attachment>(`/attachments`, form, opts)
  },
  sla: (ticket: string, o: Sig = {}) => api.get<TicketSLADetail>(`${t(ticket)}/sla`, o),
  queueCounts: (o: Sig = {}) => api.get<QueueCounts>(`/queue-counts`, o),
  statuses: (o: Sig = {}) => api.get<ItemsResponse<WorkflowStatus>>(`/statuses`, o),
  requestTypes: (o: Sig = {}) =>
    api.get<ItemsResponse<RequestType>>(`/request-types`, o),
  /** Authenticated download of `attachment.downloadUrl` (an <a href> cannot carry the bearer token). */
  async downloadAttachment(downloadUrl: string): Promise<Blob> {
    const res = await rawRequest('GET', apiPath(downloadUrl))
    return res.blob()
  },
}

export const adminApi = {
  project: (o: Sig = {}) => api.get<Project>('/project', o),
  updateProject: (project: Pick<Project, 'version'>, patch: { requesterAcknowledgement?: boolean }) =>
    api.patch<Project>('/project', patch, { ifMatch: project.version }),
  groups: (cursor: string | null = null, o: Sig = {}) => api.get<Page<Group>>(`/groups`, { ...o, query: { cursor, limit: 50 } }),
  /** Every group (all pages); for pickers that must offer each group (transfer dialog, a user's group boxes). */
  async allGroups(o: Sig = {}): Promise<Group[]> {
    const out: Group[] = []
    let cursor: string | null = null
    do {
      const page: Page<Group> = await api.get<Page<Group>>(`/groups`, { ...o, query: { cursor, limit: 100 } })
      out.push(...page.items)
      cursor = page.nextCursor
    } while (cursor)
    return out
  },
  group: (id: string, o: Sig = {}) => api.get<Group>(`/groups/${seg(id)}`, o),
  createGroup: (input: { name: string; description?: string }) =>
    api.post<Group>(`/groups`, input, { idempotencyKey: true }),
  /** `409 groupNameTaken` (unique); Triage may be renamed but stays protected. */
  updateGroup: (group: Pick<Group, 'id' | 'version'>, patch: { name?: string; description?: string; active?: boolean }) =>
    api.patch<Group>(`/groups/${seg(group.id)}`, patch, { ifMatch: group.version }),
  /** The member directory, searched and narrowed on the server (`q`: name, email or username substring). */
  members: (query: MemberQuery = {}, o: Sig = {}) =>
    api.get<Page<Member>>(`/members`, {
      ...o,
      query: {
        q: query.q || undefined,
        groupId: query.groupId,
        userId: query.userIds?.join(','),
        active: query.active ? 'true' : undefined,
        cursor: query.cursor,
        limit: query.limit ?? 50,
      },
    }),
  users: (cursor: string | null = null, o: Sig = {}) => api.get<Page<UserSummary>>('/users', { ...o, query: { cursor, limit: 50 } }),
  /** Without `username` the server derives one from the email address. `409 usernameTaken` / `emailTaken`. */
  createUser: (input: NameParts & { email: string; username?: string; password?: string; isAdmin?: boolean; sendInvitation?: boolean }) =>
    api.post<UserSummary>('/users', input, { idempotencyKey: true }),
  /**
   * `409 usernameTaken`; `409 versionConflict` when the user changed meanwhile; `409 lastAdmin` when no
   * active administrator would remain; `409 passwordRequired` when enabling a user who never set a password.
   * Disabling revokes every session of the user and releases their open tickets.
   */
  updateUser: (
    user: Pick<UserSummary, 'id' | 'version'>,
    patch: Partial<NameParts> & { username?: string; status?: 'active' | 'disabled'; isAdmin?: boolean },
  ) =>
    api.patch<UserSummary>(`/users/${seg(user.id)}`, patch, { ifMatch: user.version }),
  user: (userId: string, o: Sig = {}) => api.get<UserSummary>(`/users/${seg(userId)}`, o),
  /** Sets a password; an invited user becomes active and every session of the user is revoked. */
  setUserPassword: (userId: string, password: string) => api.put<void>(`/users/${seg(userId)}/password`, { password }),
  /** The active groups the user belongs to. */
  userGroups: (userId: string, o: Sig = {}) => api.get<{ groups: GroupRef[] }>(`/users/${seg(userId)}/groups`, o),
  /** Makes the user's active-group memberships exactly `groupIds`; removals release ineligible assignments. */
  setUserGroups: (userId: string, groupIds: string[]) =>
    api.put<{ groups: GroupRef[] }>(`/users/${seg(userId)}/groups`, { groupIds: groupIds }),
  /** `404 invitationNotFound` when there is no pending (unexpired, unused) invitation. */
  invitation: (userId: string, o: Sig = {}) => api.get<Invitation>(`/users/${seg(userId)}/invitation`, o),
  /** Send or resend; older links are revoked. */
  sendInvitation: (userId: string) => api.post<Invitation>(`/users/${seg(userId)}/invitation`),
  revokeInvitation: (userId: string) => api.delete<void>(`/users/${seg(userId)}/invitation`),
  resetUserMfa: (userId: string) => api.delete<void>(`/users/${seg(userId)}/mfa`),
  integrations: (o: Sig = {}, cursor?: string | null) => api.get<Page<Integration>>(`/integrations`, cursor ? { ...o, query: { cursor } } : o),
  integrationScopes: (o: Sig = {}) => api.get<{ items: IntegrationScope[] }>('/integration-scopes', o),
  createIntegration: (input: { name: string; scopes: string[] }) =>
    api.post<IntegrationCreated>(`/integrations`, input, { idempotencyKey: true }),
  /** Name only; scopes stay as issued. `409 integrationRevoked`. */
  renameIntegration: (integration: Pick<Integration, 'id' | 'version'>, name: string) =>
    api.patch<Integration>(`/integrations/${seg(integration.id)}`, { name }, { ifMatch: integration.version }),
  deleteIntegration: (id: string) =>
    api.delete<void>(`/integrations/${seg(id)}`),
}

type DeliveryQuery = DeliveryFilters & { cursor?: string | null; limit?: number }
const dq = (q: DeliveryQuery) => ({ channel: q.channel, status: q.status, eventId: q.eventId, cursor: q.cursor, limit: q.limit })

/** Settings (admins). */
export const settingsApi = {
  get: (o: Sig = {}) => api.get<Settings>('/settings', o),
  /** Full document (see `Settings`); `If-Match` is the loaded `configVersion`. */
  put: (body: Record<string, unknown>, configVersion: number) =>
    api.put<Settings>('/settings', body, { ifMatch: configVersion }),
  testEmail: (to: string) => api.post<{ delivery: Delivery }>('/settings/smtp/test', { to }),
}

/** All notification deliveries (admins). */
export const deliveriesApi = {
  list: (q: DeliveryQuery, o: Sig = {}) => api.get<Page<Delivery>>('/deliveries', { ...o, query: dq(q) }),
  get: (id: string, o: Sig = {}) => api.get<Delivery>(`/deliveries/${seg(id)}`, o),
  retry: (id: string) => api.post<Delivery>(`/deliveries/${seg(id)}/retry`),
}

/** Audit log (admins), newest first. */
export const auditApi = {
  list: (q: AuditFilters & { cursor: string | null; limit: number }, o: Sig = {}) =>
    api.get<Page<AuditEvent>>('/audit-events', {
      ...o,
      query: {
        actorType: q.actorType || undefined,
        actorId: q.actorId || undefined,
        action: q.action || undefined,
        ticket: q.ticket || undefined,
        from: q.from || undefined,
        to: q.to || undefined,
        cursor: q.cursor,
        limit: q.limit,
      },
    }),
  actions: (o: Sig = {}) => api.get<{ items: AuditAction[] }>('/audit-actions', o),
  actors: (q: { q?: string; limit?: number }, o: Sig = {}) =>
    api.get<AuditActorList>('/audit-actors', { ...o, query: { q: q.q || undefined, limit: q.limit } }),
}

const wh = (id: string) => `/webhooks/${seg(id)}`

/** Webhook endpoints (admins). */
export const webhooksApi = {
  eventTypes: (o: Sig = {}) => api.get<ItemsResponse<WebhookEventType>>('/webhook-event-types', o),
  list: (cursor: string | null = null, o: Sig = {}) => api.get<Page<Webhook>>(`/webhooks`, { ...o, query: { cursor, limit: 50 } }),
  get: (id: string, o: Sig = {}) => api.get<Webhook>(wh(id), o),
  create: (input: WebhookInput) =>
    api.post<Webhook>(`/webhooks`, input, { idempotencyKey: true }),
  update: (hook: Webhook, input: WebhookInput) =>
    api.patch<Webhook>(wh(hook.id), input, { ifMatch: hook.version }),
  remove: (id: string) => api.delete<void>(wh(id)),
  test: (id: string) => api.post<{ delivery: Delivery }>(`${wh(id)}/test`),
  deliveries: (id: string, q: DeliveryQuery, o: Sig = {}) =>
    api.get<Page<Delivery>>(`${wh(id)}/deliveries`, { ...o, query: { ...dq(q), channel: undefined } }),
  delivery: (id: string, deliveryId: string, o: Sig = {}) =>
    api.get<Delivery>(`${wh(id)}/deliveries/${seg(deliveryId)}`, o),
  retryDelivery: (id: string, deliveryId: string) =>
    api.post<Delivery>(`${wh(id)}/deliveries/${seg(deliveryId)}/retry`),
}

// ---- Tags, canned replies, SLA, reports. ----

export const tagsApi = {
  /** The tag catalog (admin-managed), with the live tickets the caller can read carrying each tag. */
  list: (o: Sig = {}) => api.get<ItemsResponse<TagCount>>(`/tags`, o),
  /** Admins: add a tag to the catalog (normalized by the server). */
  create: (name: string) => api.post<TagCount>(`/tags`, { name }, { idempotencyKey: true }),
  /** Admins: delete a tag; it is removed from every ticket carrying it. */
  remove: (name: string) => api.delete<void>(`/tags/${seg(name)}`),
}

const hr = (id: string) => `/hold-reasons/${seg(id)}`
export const holdReasonsApi = {
  /** Every staff member lists the reasons; `inUse` is filled for admins. */
  list: (o: Sig = {}) => api.get<ItemsResponse<HoldReason>>(`/hold-reasons`, o),
  create: (input: HoldReasonInput) => api.post<HoldReason>(`/hold-reasons`, input, { idempotencyKey: true }),
  update: (reason: HoldReason, input: HoldReasonInput) => api.patch<HoldReason>(hr(reason.id), input, { ifMatch: reason.version }),
  /** Refused with `holdReasonInUse` while a ticket is on hold with the reason. */
  remove: (reason: HoldReason) => api.delete<void>(hr(reason.id), undefined, { ifMatch: reason.version }),
}

const og = (id: string) => `/organisations/${seg(id)}`
export const organisationsApi = {
  /** Staff read (searching name or external reference); counts are filled for admins. Pickers use the default (active only). */
  list: (cursor: string | null = null, q = '', o: Sig = {}, active: 'true' | 'false' | 'all' = 'true') =>
    api.get<Page<Organisation>>(`/organisations`, { ...o, query: { cursor, q: q || undefined, active: active === 'true' ? undefined : active, limit: 50 } }),
  get: (id: string, o: Sig = {}) => api.get<Organisation>(og(id), o),
  create: (input: OrganisationInput) => api.post<Organisation>(`/organisations`, input, { idempotencyKey: true }),
  update: (org: Organisation, input: OrganisationInput) => api.patch<Organisation>(og(org.id), input, { ifMatch: org.version }),
  /** Refused with `organisationInUse` while a contact or ticket references it. */
  remove: (org: Organisation) => api.delete<void>(og(org.id), undefined, { ifMatch: org.version }),
}

const mc = (id: string) => `/macros/${seg(id)}`
export const macrosApi = {
  list: (o: Sig = {}) => api.get<ItemsResponse<Macro>>(`/macros`, o),
  create: (input: MacroInput) => api.post<Macro>(`/macros`, input, { idempotencyKey: true }),
  update: (macro: Macro, input: MacroInput) =>
    api.patch<Macro>(mc(macro.id), input, { ifMatch: macro.version }),
  remove: (id: string) => api.delete<void>(mc(id)),
  /** The body with its variables filled in for the ticket (nothing is posted). */
  preview: (ticket: string, macroId: string, o: Sig = {}) =>
    api.get<MacroPreview>(`${t(ticket)}/macros/${seg(macroId)}/preview`, o),
}

export const slaApi = {
  /** Version 0 (off, no targets) until first saved. */
  get: (o: Sig = {}) => api.get<SLASettings>(`/sla`, o),
  /** If-Match = loaded `version` (0 for the first save). */
  put: (input: SLAInput, version: number) => api.put<SLASettings>(`/sla`, input, { ifMatch: version }),
}

export const reportsApi = {
  summary: (range: { from?: string; to?: string }, o: Sig = {}) =>
    api.get<ReportSummary>(`/reports/summary`, { ...o, query: { from: range.from, to: range.to } }),
}

// ---- Milestone 7: full-text search. ----

export const searchApi = {
  /** Subjects and readable message bodies; `q` 1–100 characters. */
  tickets: (q: string, cursor: string | null, o: Sig = {}) =>
    api.get<SearchPage>(`/search`, { ...o, query: { q, cursor, limit: 20 } }),
}

// ---- Ticket workflow: links, follow-ups, activity, redaction, lifecycle automation. ----

export const workflowApi = {
  /** Only links whose other ticket the caller may read. */
  links: (ticket: string, o: Sig = {}) =>
    api.get<ItemsResponse<TicketLink>>(`${t(ticket)}/links`, o),
  /** `ticket` = the other ticket's key or ID; `type` = relation of this ticket to it. */
  addLink: (ticket: string, input: { type: LinkType; ticket: string }) =>
    api.post<TicketLink>(`${t(ticket)}/links`, input),
  removeLink: (ticket: string, linkId: string) =>
    api.delete<void>(`${t(ticket)}/links/${seg(linkId)}`),
  /** New ticket linked `followUpTo` the original (subject, requester, request type and group copied). */
  followUp: (ticket: string, input: FollowUpInput, idempotencyKey: string) =>
    api.post<Ticket>(`${t(ticket)}/follow-up`, input, { idempotencyKey }),
  /** New ticket linked `copiedFrom` the original (requester, CC, organisation, group, tags and first message copied). */
  copy: (ticket: string, input: CopyTicketInput, idempotencyKey: string) =>
    api.post<Ticket>(`${t(ticket)}/copy`, input, { idempotencyKey }),
  /** Follows a `createFollowUp` problem action (its href is an absolute API path). */
  followUpAt: (href: string, input: FollowUpInput, idempotencyKey: string) =>
    api.post<Ticket>(apiPath(href), input, { idempotencyKey }),
  /** Oldest first. Staff only. */
  /** Newest first by default; `action` keeps one exact action (e.g. `message.redacted`). */
  activity: (ticket: string, cursor: string | null, o: Sig & { order?: 'asc' | 'desc'; action?: string; limit?: number } = {}) =>
    api.get<Page<ActivityItem>>(`${t(ticket)}/activity`, {
      ...(o.signal ? { signal: o.signal } : {}),
      query: { cursor, limit: o.limit ?? 50, order: o.order ?? 'desc', action: o.action },
    }),
  /**
   * Admins; irreversible. `reason` 3–1000 characters. `removeAttachments` also removes the message's
   * files (on an already redacted message: the files kept then).
   */
  redact: (ticket: string, messageId: string, reason: string, removeAttachments = false) =>
    api.post<Message>(`${t(ticket)}/messages/${seg(messageId)}/redact`, {
      reason,
      ...(removeAttachments ? { removeAttachments: true } : {}),
    }),
  automation: (o: Sig = {}) => api.get<Automation>(`/automation`, o),
  /** If-Match = loaded `version` (0 for the first save). */
  saveAutomation: (input: AutomationInput, version: number) =>
    api.put<Automation>(`/automation`, input, { ifMatch: version }),
}
