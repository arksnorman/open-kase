# open-kase REST API v1: guide

This is the human guide to the REST API: authentication, conventions, errors and the behaviour behind the main
resources. **The generated OpenAPI 3.1 document is the authority for routes, request and response shapes, status
codes, error codes, headers and permissions.** It is served at `GET /api/v1/openapi.json`, committed as
[`openapi.json`](openapi.json) and regenerated with `open-kase openapi --out docs/openapi.json` (a test fails when
it is stale or a route is missing). Where this guide and `openapi.json` disagree, `openapi.json` wins.

TypeScript types for a client: `npx openapi-typescript docs/openapi.json -o schema.d.ts` (`ui/` does this with
`npm run gen:api`).

## Model

- One installation has one workspace and **one project**. There is no project prefix on routes and no project
  switcher. Tickets are identified by number, shown as `#1042`; `/tickets/{t}` accepts the number, `#1042`
  (URL-encoded `%231042`) or the ID.
- Two roles: **admin** (everything an agent can do, plus users, groups, settings, SLA, automation, canned replies,
  tags, hold reasons, integrations, webhooks, deliveries, audit log, reports) and
  **agent** (works the queues of their groups: claim, reply, notes, status, assign and transfer, contacts). Users carry `isAdmin`.
- Groups are team queues. A ticket has exactly one current group; claiming keeps the group.
- There are no customer logins and no customer portal. Requesters are **contacts** (name, email, optional
  `externalId`). External systems use the API with integration tokens.
- Message audiences are `public` (the requester) and `agent` (staff only).
- Tickets are never deleted. Archived is the terminal state.
- Only email and outbound webhooks exist as notification channels. There is no built-in AI.

## Conventions

- JSON in and out (`Content-Type: application/json`), camelCase field names, UTC RFC 3339 timestamps, opaque
  string IDs. Nullable text fields are `null` when empty.
- **Lists** return `{ "items": [...], "nextCursor": string | null }` and take `?cursor=&limit=` (default 25, max
  100). Treat the cursor as opaque; a bad cursor is `422` on field `cursor`.
- **Strict bodies.** Unknown fields, `$`-prefixed or dotted keys, oversized input and invalid enum values are
  `422 validationFailed` (field code `unknownField`, ...).
- **Versions.** Mutable resources carry `version` (integer). Mutations send `If-Match: <version>` (`3` and `W/"3"`
  are accepted). Missing is `428 preconditionRequired`; stale is `409 versionConflict`.
- **Idempotency.** `POST` creates accept `Idempotency-Key` (1-200 printable ASCII), scoped to the caller and route
  and kept 24 hours. Same key and same body replays the stored response with `Idempotent-Replayed: true`; same key
  with a different body is `409 idempotencyKeyReused`; while the first request runs it is
  `409 idempotencyInProgress`. Failed (non-2xx) requests release the key. A key whose stored response was removed by
  a redaction answers `409 idempotencyKeyUsed` (the call is never repeated).
- **Errors** are problem details:

```json
{ "type": "urn:open-kase:error:validation", "title": "...", "status": 422, "code": "validationFailed",
  "detail": "...", "requestId": "...",
  "errors": [{ "field": "priority", "code": "outOfRange", "message": "..." }] }
```

  `code` is stable and meant for programs; `title`, `detail` and field `message` are for people. A problem may
  carry `actions: [{ rel, title, method, href }]`: `ticketArchived` always offers `createFollowUp`
  (`POST /api/v1/tickets/{number}/follow-up`); show it as a button and never retry the original request.
  Common statuses: `401` (`authenticationRequired`, `invalidToken`, `tokenExpired`, `sessionRevoked`,
  `accountInactive`, with `WWW-Authenticate: Bearer`), `403` (`forbidden`, `insufficientScope`, `userRequired`,
  `csrfFailed`, `originRejected`, `mfaEnrolmentRequired`), `404` (`ticketNotFound`, ... ; an inaccessible ticket
  is indistinguishable from a missing one), `409` (`versionConflict`, `ticketArchived`, `invalidTransition`,
  `ticketAlreadyClaimed`, ...), `410` (`attachmentRemoved`), `413`, `415`, `422`, `428`, `429` and `503
  notInitialized` before `init` has run.
- **Rate limits.** `429 rateLimited` with `Retry-After` (seconds) when a per-principal limit (per client IP on
  `/auth/*`) is exceeded. Classes and defaults are in `docs/operations.md`. `GET /events` allows 10
  concurrent streams per principal (`429 tooManyStreams`); `POST /attachments` allows 50 unattached uploads
  (`429 tooManyPendingUploads`). Honour `Retry-After`.
- **Security headers.** Every response carries `Cross-Origin-Opener-Policy`, `Cross-Origin-Resource-Policy` and a
  restrictive `Permissions-Policy`; the SPA is served with a strict CSP.
- Health probes (no auth): `GET /health/live`, `GET /health/ready`. `GET /api/v1/meta` returns the product name
  and API version.

## Authentication

### User sessions (agents and admins)

The SPA keeps the access token in memory. The refresh token is an HttpOnly cookie `kase_refresh` (Path
`/api/v1/auth`, SameSite=Strict, `Secure` unless `APP_ENV=development`). A readable cookie `kase_csrf` holds the
CSRF token. Cookie routes (`refresh`, `logout`) require header `X-CSRF-Token` equal to `kase_csrf` and an `Origin`
that matches the request `Host` or the configured `product.baseUrl`.

| Route | Notes |
| --- | --- |
| `POST /auth/login` | `{ login, password }`; `login` is an email (contains `@`) or a username. `200 { accessToken, expiresIn, user, mfaRequired, mfaEnrolmentRequired }` and sets cookies. JSON only (`415` otherwise); cross-site requests are refused |
| `POST /auth/mfa` | `{ mfaToken, code }` or `{ mfaToken, recoveryCode }`; completes a sign-in that answered `mfaRequired: true` |
| `POST /auth/refresh` | rotates the refresh cookie; `200` like login |
| `POST /auth/logout` | `204`, revokes the session, clears cookies |
| `GET/PATCH /me`, `PUT /me/preferences`, `POST /me/password` | own profile (name parts, theme, signature, email preferences) and password |

- The access token is an EdDSA JWT (`typ` `at+jwt`), 10 minutes by default (`auth.accessTokenTtlSeconds`). The
  session, the user's active state and group memberships are re-checked on every request, so logout,
  deactivation, password change and membership removal apply at once. Sessions end after
  `auth.refreshTokenTtlHours` (default 14 days).
- **Refresh reuse.** Presenting an already-rotated refresh token revokes the whole session
  (`401 refreshTokenReused`). For 10 seconds after a rotation the just-rotated token is still accepted as long as
  its successor was not used; that answer carries a new access token and no `Set-Cookie`, so clients must keep the
  refresh token from the response that rotated it.
- **Login failures.** `401 invalidCredentials` is identical for an unknown account, wrong password and
  disabled or invited accounts; `429 loginThrottled` with `Retry-After`. Failures are counted per account, per
  client (IPv4 address or IPv6 /64) and per IP over 15 minutes; behind a proxy not listed in
  `OPEN_KASE_TRUSTED_PROXIES` all requests share one client.
- **MFA (TOTP).** RFC 6238, 6 digits, 30 s, each time step accepted once. A user with MFA gets
  `{ mfaRequired: true, mfaToken, mfaExpiresIn: 300 }` and no cookies until `POST /auth/mfa` succeeds
  (`401 mfaChallengeInvalid`, `401 invalidMfaCode`, `429 mfaThrottled`). Own MFA: `GET /me/mfa`,
  `POST /me/mfa/totp/enrol` and `/confirm` (confirm returns 10 recovery codes once), `POST /me/mfa/recovery-codes`,
  `POST /me/mfa/disable` (`409 mfaRequiredByPolicy` for admins while required). Both take `{ password, code }` or
  `{ password, recoveryCode }`: the current password plus a current TOTP code (a used step is refused) or an unused
  recovery code, which is consumed (`422` on `password` or `code`, `429 mfaThrottled`). `DELETE /users/{u}/mfa` lets an
  admin reset another user's MFA.
- **Required admin MFA.** With `auth.requireAdminMfa` on (the `init` default outside development and test), an
  admin without MFA gets `mfaEnrolmentRequired: true` and every route except `GET /me`, `GET /me/mfa` and the two
  enrolment routes answers `403 mfaEnrolmentRequired` (MCP included). Enable it before exposing an installation.
- **Password reset.** `POST /auth/password-reset/request { email }` always answers `202`; an active user gets a
  single-use link valid 30 minutes (3 requests per address and 20 per IP per hour, `429 resetThrottled`).
  `POST /auth/password-reset/complete { token, password }` revokes every session of the user.
- **Invitations.** `POST /users` with `sendInvitation: true` (no `password`) creates an `invited` user and emails a
  single-use link valid 72 hours. `GET/POST/DELETE /users/{u}/invitation` inspect, resend and revoke it;
  `POST /auth/invitations/accept { token, password, ... }` activates the user.
- **Usernames.** Optional on `POST /users` (3-32 characters, lower-case letters, digits, `.`, `_`, `-`, starting
  with a letter; derived from the email when omitted); unique (`409 usernameTaken`).
- The CLI signs in through the same routes (`docs/cli.md`).

### API tokens (integrations)

There is **one kind** of API token. An admin creates it with `POST /integrations { name, scopes }` (or in the web
app under Settings, Integrations); the token (`kase_live_...`) is shown once, stored only as a SHA-256 hash, and
refused from the next request after revocation. Send `Authorization: Bearer kase_live_...`.
`GET /integration-scopes` lists every scope as `{ name, staff, description }`. Unknown fields are `422`.

| Scope | Grants |
| --- | --- |
| `tickets:read` | tickets and their public messages and attachments; with a staff scope also groups, the member directory and canned replies |
| `tickets:create` | create tickets and follow-ups (with `tickets:read`) for a requester contact; `priority` applies when the token is staff, otherwise it is recorded as `requestedPriority`. The first message is authored by the bot only when the token holds `tickets:comment` (or `tickets:all`), otherwise by the requester contact |
| `tickets:replyAsRequester` | public replies **as the ticket's requester** (`author.context = "contact"`; may reopen the ticket, owes a response, never a first response); also lets a requester-side token **resolve** any ticket it can read (`POST /tickets/{t}/transitions {"action":"resolve"}`, no `If-Match` needed; `contactId` names the teammate who did it) |
| `tickets:comment` | public replies **as staff** under the token's name (`author.context = "bot"`; counts as a staff reply for the SLA) |
| `tickets:internal` | read and write agent notes (the only way a token sees private content) |
| `tickets:write` | status, priority, tags, request type, organisation, group and assignee |
| `tickets:all` | every ticket scope **except** `tickets:replyAsRequester` |
| `contacts:read`, `contacts:write` | read; create and update contacts (`contacts:write` may link an organisation) |
| `tags:read`, `tags:write` | list the tag catalog; create and delete tags |
| `holdReasons:read`, `holdReasons:write` | list; create, change and delete hold reasons |
| `organisations:read`, `organisations:write` | list and read; create, change and delete organisations |

The tag, hold reason and organisation scopes are not part of `tickets:all` and do not make a token staff.

- A **staff token** holds `tickets:comment`, `tickets:internal`, `tickets:write` or `tickets:all`. It gets the
  staff projection of tickets (assignee, tags, SLA, `version`, agent notes with `tickets:internal`) and acts as a
  bot under its own name. Any other token is **requester-side**: it sees public messages only, no assignee, tags,
  SLA or staff activity, `version` is always `0`, and its writes are authored by the requester contact.
- A token never exceeds its scopes (`403 insufficientScope`) and never exceeds what a human agent may do: it cannot
  claim, release, watch, draft, schedule, link, read the activity timeline or queue counts, redact, read reports or
  use administration routes (`403 userRequired`). One addition: with `tickets:write` a token may transfer and assign
  tickets (the assignee must be an active member of the target group, `422 assigneeId notEligible`; a bot is never
  an assignee). User-only views (`myWork`, `unassigned`, `watching`, `dueSoon`) are refused.
- **Message author** (`POST /tickets/{t}/messages`): optional `as` = `staff` | `requester`. `requester` needs
  `tickets:replyAsRequester`, a public message and no `thenStatusId`. `staff` needs `tickets:comment` for public
  replies and `tickets:internal` for notes. Omitted: `staff` when the token holds `tickets:comment` (or
  `tickets:all`), otherwise `requester`. Users always post as themselves.
  With `as: "requester"`, the optional `contactId` authors the reply as another contact of the ticket's
  organisation (a teammate of the requester) instead of the requester: `author.id` and `author.name` are that
  contact's. The ticket needs an organisation and the contact must exist in the project with that organisation, else
  `422` with field error `contactNotInOrganisation`. `contactId` is
  accepted when `as` is `"requester"` and also when `as` is omitted and defaults to requester (a token with neither
  `tickets:comment` nor `tickets:all`); with `as: "staff"` (or a default of `staff`) it is `422 notAllowed`. The requester's own ID equals
  omitting it. An unknown contact ID answers `contactNotInOrganisation` as well, so contact IDs cannot be probed.
  The audit event of the message records `contact:<id>`. The reply is otherwise a
  requester reply (reopens, ends a hold, never a first response, no `thenStatusId`).
- Requester-side tokens never see staff identities: `author.id` of staff messages is `""` and `assignee` is `null`. Staff tokens see assignee and author IDs.
- Every write made with a token (REST or MCP) is audited with the token as actor (`type: service`) plus the
  `scopes` that allowed it, never message bodies or secrets.
- The same tokens and scopes work at `/mcp` (`docs/mcp.md`).

## Projects, groups, people

| Route | Notes |
| --- | --- |
| `GET/PATCH /project` | the deployment's project, created by `init`; `PATCH` (admin) takes only `requesterAcknowledgement` (email requesters "we received your request"). The product name and description are settings |
| `GET/POST /groups`, `GET/PATCH/DELETE /groups/{g}` | `{ name, description? }`; the seeded **Triage** group may be renamed but never deactivated or deleted (`409 groupProtected`); delete only when empty |
| `GET /groups/{g}/members`, `PUT/DELETE /groups/{g}/members/{u}` | membership (admin); removing a member releases their ineligible assignments through a background job |
| `GET/PUT /users/{u}/groups` | read or replace a user's groups (`{ groupIds }`) |
| `GET /members` | read-only directory of users with `role`, groups and active state; filters `q`, `groupId`, `userId`, `active` |
| `GET/POST /users`, `GET/PATCH /users/{u}`, `PUT /users/{u}/password` | admin user management: `{ email, username?, givenName, middleName?, familyName?, password?, isAdmin?, sendInvitation? }`; `name` is read-only |
| `GET/POST /contacts`, `GET /contacts/{c}` | requesters; `GET /contacts?q=&externalId=` (`externalId` is an exact match, `acct-1` never matches `acct-10`; combined with `q` it narrows further); `POST` upserts by `externalId` first, then by email (a pair that identifies two different contacts, or an email that already carries another `externalId`, is `409`); optional `externalId` and `organisationId` (`""` clears, unknown ID is `422`, inactive organisation `422 organisationInactive`) |
| `GET/POST /organisations`, `GET/PATCH/DELETE /organisations/{o}` | customer organisations (`name`, `externalRef`, `active`); the organisation is set explicitly on contacts and tickets and never inferred. `409 organisationExists`, `409 organisationRefTaken`, `409 organisationInUse` on delete while any contact or ticket references it (deactivate instead). Counts only for managers |
| `GET/POST /integrations`, `PATCH/DELETE /integrations/{i}` | API tokens (admin, users only); `PATCH` renames; scopes and token never change after issue; `409 integrationRevoked` |
| `GET /statuses`, `GET /request-types` | built-in statuses in order: Open, Awaiting customer, On hold, Resolved, Archived; request types are separate from priority |

## Tickets

Priorities are integers 1-4 (1 Critical, 2 High, 3 Normal, 4 Low). Status categories: `open`,
`waitingCustomer`, `onHold`, `resolved`, `archived`. There is no "In progress" status: claiming or assigning keeps a
ticket Open. Full shapes (`Ticket`, `Message`, `Attachment`, `Draft`, ...) are in `openapi.json`.

| Route | Notes |
| --- | --- |
| `GET /tickets` | `?view=&q=&groupId=&cursor=&limit=` plus filters. Views: `all` (default), `myWork`, `unassigned` (my groups), `watching`, `awaitingCustomer`, `onHold`, `dueSoon`, `resolved`, `archived`. `q` is a ticket number or a subject substring. Sorted by `updatedAt` descending. Filters (comma-separated, combinable with any view): `statusId`, `organisationId` and `requesterId` (one contact ID: that contact's tickets) work for staff and for requester-side tokens (they only narrow what the token already reads); `priority`, `tag` (all listed), `requestTypeId`, `assignee=me\|unassigned\|<id>` and `due=dueSoon\|breached\|met\|missed\|running` are staff only (`403 userRequired` for requester-side tokens) |
| `POST /tickets` | `{ subject, body, requester: { contactId } \| { name, email, externalId?, organisationId? }, priority?, requestTypeId?, groupId?, attachmentIds?, organisationId?, ccEmails? }` -> `201` + `Location`. The ticket enters the group (default Triage) unassigned. `ccEmails`: up to 10 addresses copied on requester emails. Requires `tickets:create` for tokens; `Idempotency-Key` supported |
| `GET /tickets/{t}` | one ticket |
| `PATCH /tickets/{t}` | `{ priority?, requestTypeId?, tags?, organisationId?, ccEmails? }` + **If-Match**. `subject` is locked for staff (`403 subjectLocked`); only the token that created a ticket may change its subject. Signed-in users cannot change `organisationId` on a ticket created through the API or MCP (`403 organisationManagedByIntegration`); tokens can. Tags replace the set and each added tag must be in the catalog (`422 unknownTag`) |
| `POST /tickets/{t}/claim` | **If-Match**, group members only. Atomic: exactly one concurrent claim wins, the others get `409 ticketAlreadyClaimed`. Keeps status and group; non-member `403 notGroupMember` |
| `POST /tickets/{t}/release` | `{ reason }` (3-500 characters, required) + If-Match; assignee or admin; `409 ticketNotAssigned` |
| `POST /tickets/{t}/assignments` | `{ groupId, assigneeId? }` + If-Match. Absent `assigneeId` keeps the assignee when still an active member of the target group, otherwise clears; `null` unassigns; an ID assigns (`422 assigneeId notEligible`) |
| `POST /tickets/{t}/transitions` | `{ action, statusId?, holdReasonId?, note?, reason?, contactId? }` + If-Match. `resolve`; `reopen` (Resolved to Open); `archive` (Resolved to Archived, final); `hold` (Open or Awaiting customer, needs `holdReasonId`); `setStatus` (an open or awaiting-customer status; any other status is `422` with field `statusId` code `notAllowed`). A transition not allowed from the current status (including a staff resolve of an already Resolved ticket) is `409 invalidTransition`. A requester-side token holding `tickets:replyAsRequester` may send `resolve` only: it sees no version, so `If-Match` is not required (and ignored); it is audited with the token as actor and behaves like a staff resolve (`ticket.statusChanged` with transition `resolve`, SLA stopped, reminders cleared), except that the "resolved" email goes to the rest of the customer side (never the person who resolved it) and that resolving an already Resolved ticket answers `200` with the ticket and writes nothing (an archived ticket is `409 ticketArchived`). `contactId` (requester-side resolve only, otherwise `422 notAllowed`) names the contact of the ticket's organisation who resolved it (`422 contactNotInOrganisation` when it is not one; the requester's own ID counts as the requester); the `ticket.resolved` audit event records it (`contact:<id>`) and the `ticket.statusChanged` webhook carries it as `contactId`. Archive, reopen, hold and setStatus stay staff only (`403 userRequired`); the requester reopens by replying. Without `tickets:replyAsRequester` (and without a staff scope) every action is `403 userRequired` |
| `GET /tickets/{t}/messages` | oldest first; `?visibility=public\|agent`. Integrations receive public messages only |
| `POST /tickets/{t}/messages` | `{ visibility, body, attachmentIds?, expectedLastPublicId?, thenStatusId?, addSignature?, as?, contactId? }` -> `201 Message` plus the updated `ticket`. See below |
| `GET/PUT/DELETE /tickets/{t}/drafts/{visibility}` | the caller's private draft per audience; `PUT` takes optional `If-Match` on the draft version |
| `GET /tickets/{t}/watchers`, `PUT/DELETE /tickets/{t}/watchers/me` | watching grants no access; each watcher's access is re-checked when listed |
| `GET/POST /tickets/{t}/links`, `DELETE /tickets/{t}/links/{l}` | `relatedTo`, `duplicateOf`, `followUpTo` (addable) and `copiedFrom` (created by copy). A link is shown only when the caller can read the other ticket |
| `POST /tickets/{t}/follow-up` | new ticket linked `followUpTo` the original (works on archived tickets); copies subject, requester, organisation, request type, group and the CC list; default body `Follow-up to #<number>.` Optional `contactId` opens it as another contact of the original's organisation (`422 contactNotInOrganisation` otherwise); only when the first message is written as the requester (a token with neither `tickets:comment` nor `tickets:all`), else `422 notAllowed`. That contact is then the requester of the new ticket and the author of its first message; the organisation stays the original's, and the copied CC list drops the contact's own address (the original requester is not added). No `contactId` is recorded on `ticket.created` or its webhook |
| `POST /tickets/{t}/copy` | new Open ticket linked `copiedFrom`; copies requester, CC, organisation, priority, request type, group, tags, first public message and attachments; not assignee, watchers, notes or history |
| `GET /tickets/{t}/activity` | staff only; the ticket's audit timeline, field names only, plus marks such as `visibility:<audience>`, `links:<type>` and `contact:<id>` (the contact who acted for the customer side instead of the requester); `?order=desc&action=` |
| `POST /tickets/{t}/messages/{msg}/redact` | admin; see Redaction |
| `GET /queue-counts` | counts for the sidebar queues (users) |
| `POST /attachments`, `GET /attachments/{a}/download` | see Attachments |

**Messages.** The audience is explicit and never inferred. Body 1-100,000 code points (or the configured lower
limit), raw Markdown stored as sent. `expectedLastPublicId` different from the ticket's `lastPublicMessageId` is
`409 newerReplyExists` (drafts untouched). A public reply from the requester on an Awaiting customer, On hold or
Resolved ticket reopens it to Open (assignee kept when still eligible) and ends a hold. Agent notes never change
status or public activity. `thenStatusId` sets a status in the same transaction (Resolved, or an open or
awaiting-customer status; Archived and On hold are `422 notAllowed`); a rejected transition posts nothing. Posting
honours `Idempotency-Key` inside its transaction.

**Archived tickets** are immutable through ordinary operations: any change is `409 ticketArchived`. Create a
follow-up instead.

**On hold.** `ticket.hold = { reasonId, reasonName, until }` (staff only). The hold lasts exactly the reason's
`maxDays`; a durable job returns the ticket to Open at `until` (assignee kept, audited `ticket.holdEnded`, the
assignee is emailed). A staff status change, a requester reply or a requester-side resolve ends it earlier. The requester is never notified,
the first-response SLA keeps running, and reminders and auto-resolve do not apply while on hold.

**Hold reasons.** `GET/POST /hold-reasons`, `PATCH/DELETE /hold-reasons/{h}`: `{ name, maxDays }` (name 1-80,
unique; `maxDays` 1-365; at most 100). A new `maxDays` affects only later holds; delete is
`409 holdReasonInUse` while a ticket is on hold with the reason. `init` seeds six common reasons once.

**Attachments.** `POST /attachments` (`multipart/form-data`, field `file`) stages an upload for 24 hours (10 MiB
per file by default, `413 attachmentTooLarge`); reference the IDs in `attachmentIds` (at most 5 per message, 50 MiB
total) when posting. Staged files are visible to their uploader only. Downloads stream with
`Content-Disposition: attachment`, `nosniff` and `no-store`; unusual content types are served as
`application/octet-stream`. Files of agent notes are `404` for integrations. Attachments are stored in GridFS.

**Scheduled messages.** `GET/POST /tickets/{t}/scheduled-messages`, `PATCH ...{s}`, `POST ...{s}/cancel`,
`POST ...{s}/send` (users). Create takes `{ visibility, body, sendAt, attachmentIds?, expectedLastPublicId?,
thenStatusId? }`; `sendAt` is in the future and within 365 days. A durable job sends it at `sendAt` after
re-authorizing the author and re-checking the ticket, at most once. If a requester replied after it was written, or
the status change is no longer valid, it becomes `needsReview` and the author is emailed (link only) to send, edit
or cancel; if the ticket was archived or access is lost it is `cancelled` with a `reason`. Until sent it is
invisible to integrations, MCP, search, counts and the SLA.

**Redaction.** `POST /tickets/{t}/messages/{msg}/redact { reason, removeAttachments? }` (admin; allowed on archived
tickets). The body becomes `[redacted]` and `redactedAt` is set; there is no message delete. The stored
idempotent-replay response and scheduled copy of the text are removed too. With `removeAttachments: true` each
file's metadata is dropped (it becomes `{ id, removed: true }`, downloads answer `410 attachmentRemoved`) and a
durable job deletes the bytes. Emails and webhooks already delivered cannot be retracted. Errors:
`409 messageAlreadyRedacted`.

## Tags, canned replies, SLA, automation, reports

- **Tags.** `GET/POST /tags`, `DELETE /tags/{name}`. The catalog is admin-managed (up to 500; names
  `^[a-z0-9][a-z0-9_-]{0,39}$` after trimming, lower-casing and spaces becoming `-`). Deleting removes the tag from
  every ticket (archived too). `count` covers only tickets the caller can read as staff. `409 tagExists`.
- **Canned replies** (path `/macros`): `GET /macros`, `GET /macros/{m}` (staff); `POST`, `PATCH`, `DELETE` (admin;
  text only, up to 200). Fields `{ name, body, visibility: public|agent, active }`. Variables `{{requester.name}}`,
  `{{ticket.number}}` and `{{agent.name}}`. `GET /tickets/{t}/macros/{m}/preview` renders one for review; nothing
  is sent.
- **SLA.** One SLA, no name, **first response only**, counted in wall-clock time 24/7. `GET /sla` (staff),
  `PUT /sla` (admin, If-Match; the first save needs none): `{ active, warningPercent (50-95, default 80),
  targets: { "1".."4": { firstResponseMinutes } } }`. It is off until `active: true`. Each ticket cycle snapshots
  the SLA it started with. The clock runs from creation and is met only by the first public reply by staff (a user
  or a `tickets:comment` token); notes, claims and requester replies never count. Resolving or archiving before
  that stops it unmet; reopening starts nothing. `Ticket.sla` carries `state` (`ok`, `dueSoon`, `breached`, `met`),
  `dueAt` and `breached`; `GET /tickets/{t}/sla` returns the cycle history. Warnings and breaches are durable
  timers that emit `ticket.slaWarning` and `ticket.slaBreached` once per deadline (email to the assignee and group
  members, never the requester; webhooks). Turning the SLA off closes every open cycle (`closedReason: slaOff`).
  `view=dueSoon` lists tickets whose running target is due soon or breached.
- **Lifecycle automation.** `GET/PUT /automation` (read: staff; write: admin, If-Match):
  `{ enabled, customerFollowupAfterHours, maxReminders, reminderCooldownHours, autoResolveAfterHours,
  archiveAfterResolvedDays }`, all rules off (0) by default. Awaiting-customer tickets older than the follow-up
  delay get reminder emails to the requester (at most `maxReminders`, `reminderCooldownHours` apart);
  `autoResolveAfterHours` resolves a ticket that long after the last reminder, which always carries the notice
  (one extra reminder carries it when auto-resolve was turned on, or `maxReminders` lowered, after the last one went out);
  `archiveAfterResolvedDays` archives resolved tickets. A requester reply, any status change and a public staff
  reply reset the clock. Durable jobs do the work and re-check before acting.
- **Reports.** `GET /reports/summary?from=&to=` (admin): backlog, created vs resolved per day, median first
  response and resolution, SLA breaches and reopen rate over at most 92 days. Computed live and bounded
  (`kind: boundedLiveTraversal`, at most 10,000 samples, `truncated` when reached).

## Search and live updates

- `GET /search?q=&cursor=&limit=`: full-text over ticket subjects and the message bodies the caller may read
  (integrations: public only; agent notes for users and `tickets:internal` tokens). Word-based, no stemming;
  `"phrase"` and `-exclude` work; `q` is required and at most 100 characters. A ticket number is matched exactly
  and listed first. Hits carry plain-text snippets with UTF-16 highlight ranges: render them as text and wrap the
  ranges yourself. Authorization happens before counts: `total` counts only visible tickets (`totalCapped` when
  the 1,000-candidate bound is reached).
- `GET /events`: Server-Sent Events over fetch streaming with `Authorization: Bearer` (a token in the URL is
  refused). Events: `ready`, `invalidate { type, ticketId, version }`, `resync` (refetch what you show), `error`
  (credentials expired or access lost; the stream ends) and `reconnect`. Heartbeat comments every 15 seconds. Each
  change is authorized per caller; integrations receive public-audience changes only, with `version: 0`. There is
  no replay: open the stream first, then refetch. Polling the list endpoints is an equally valid client strategy.

## Settings, email, deliveries, webhooks, audit

- `GET/PUT /settings` (admin; secrets redacted): `product` (`name`, `description`, `baseUrl`, `signature`),
  `auth` (`accessTokenTtlSeconds`, `refreshTokenTtlHours`, `requireAdminMfa`), `files` (attachment and message
  limits), `notifications` (`requesterLinkBaseUrl`, `requesterEmails`; both optional, absent keeps the stored value), `smtp`. `PUT` is guarded by `configVersion` (`If-Match`); absent
  optional sections keep the stored value. Product settings live in MongoDB; running processes pick up changes.
- **SMTP.** `GET/PUT /settings/smtp`, `POST /settings/smtp/test { to }` (admin). Fields `enabled`, `host`, `port`,
  `username`, write-only `password`, `fromAddress`, `fromName`, `tlsMode` (`starttls` default, `tls`, or `none` for
  a local relay without credentials), optional `caCertificate` and `tlsSkipVerify`. Server certificates are verified
  by default. Enabling needs `host`, `fromAddress` and `product.baseUrl`. When a password is stored and the
  destination weakens or changes (host, port, username, `tlsMode`, `tlsSkipVerify` on, new CA), the password must
  be sent again (`422 smtp.password required`). Mail is sent with `github.com/wneessen/go-mail`.
- **Who gets email.** Unless the switch **Email contacts and CC addresses** (`notifications.requesterEmails`,
  Settings, Configuration, General; default on) is off, requesters (contacts) and CC addresses get a received notice, a "new reply" notice without the reply
  text, and resolved/archived notices; follow-up reminders; links use `notifications.requesterLinkBaseUrl` when set. With the switch off no email goes to
  contacts or CC addresses; each is recorded as a skipped delivery with class `requesterEmailsOff` (checked at send
  time, also for already queued jobs), and staff emails are unchanged. While SMTP is disabled the delivery is recorded
  as `smtpNotConfigured` instead. A public reply or a resolve made by the customer side (the requester, or a teammate named by `contactId`) emails the rest of the customer
  side, never the person who acted: the CC addresses when the requester acted, the requester and the CC addresses
  except the teammate's own address when a teammate did. Staff get links to
  the SPA: the assignee, watchers (`watcherUpdates` preference) and the group queue while unassigned
  (`queueUpdates`). Agent notes notify with a link only, never the text. Authors never receive their own notice;
  recipient access is re-checked just before sending. Mail has a stable `Message-ID`; delivery is at least once.
- **Deliveries.** `GET /deliveries`, `GET /deliveries/{d}`, `POST /deliveries/{d}/retry` (admin): email and
  webhook records with `status` `pending`, `retrying`, `sent`, `failed` (dead-lettered), `unknownOutcome` (SMTP may
  have accepted it; not retried automatically), `skipped`, `skippedUnconfigured`. Retries use 1 minute, 5 minutes,
  30 minutes, 2 hours and 12 hours, with jitter.
- **Webhooks.** `GET/POST /webhooks`, `GET/PATCH/DELETE /webhooks/{w}`, `POST /webhooks/{w}/test`,
  `GET /webhooks/{w}/deliveries[/{d}]`, `POST /webhooks/{w}/deliveries/{d}/retry`, `GET /webhook-event-types`
  (admin for management). An endpoint has `name`, `url`, `eventTypes`, `active`, `includeAgentNotes`, up to 20 extra
  `headers`, `auth` (`none`, `basic`, `token`) and `tls` (`caCertificate`, `clientCertificate`, `clientKey`).
  Credentials, header values and the client key are encrypted and write-only (responses report `...Configured`
  flags); omitting them on `PATCH` keeps the stored value, except when the destination changes (scheme, host or
  port, or a different non-empty `tls.caCertificate`): then the Basic password or Token and every secret header
  value must be sent again (`422` `required` on `auth` or `headers[i].value`), unless `auth` is set to `none`. Deliveries are **unsigned**. Receiver details, payloads
  and retry rules: `docs/webhooks.md`.
- **Audit log.** `GET /audit-events` (admin, users only) with filters `actorType`, `actorId`, `action`, `ticket`,
  `from`, `to`; `GET /audit-actions` and `GET /audit-actors` provide the catalogs. Events record changed field
  names and identifiers, never message bodies, secrets or token material.

## MCP

`POST /mcp` is a Model Context Protocol endpoint (not a REST route) that uses the same bearer credentials and
scopes. Tools, arguments and client setup: `docs/mcp.md`.
