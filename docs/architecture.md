# open-kase architecture

This document describes open-kase as built. It is a map of the code, not a specification: when it and the code
disagree, the code wins. Package paths are relative to `api/` unless they start with `ui/`.

## 1. Scope

open-kase is a self-hosted support ticket system for one team and one support project.

- **One project per deployment.** `init` creates it. There is no project switcher, project CRUD or project
  membership. Tickets are numbered `#1`, `#2`, ... (a per-project counter).
- **Two roles: admin and agent.** Groups are team queues. A ticket has exactly one current group and an optional
  assignee. The assignee must be an active member of that group. Claiming keeps the group.
- **Requesters are contacts** (name, email, optional external ID, optional organisation). They never sign in. There
  is no customer portal. Other systems (a website, a mail gateway, a monitor) create tickets and replies through
  the REST API or MCP with integration tokens.
- **Message audiences are `public` and `agent`.** A public reply reaches the requester; an agent note is staff-only.
- **No built-in AI.** External AI agents use the same REST API and MCP endpoint as any integration.
- **Notifications are email and outbound webhooks only.**
- **Tickets are never deleted.** Archived is the terminal state.
- **Single host by design.** MongoDB (single-node replica set), Redis, and one `open-kase` binary that serves the
  API and the built SPA. It is not highly available and has no multi-replica mode.

The product name lives in one place, `package/product/product.go`.

## 2. Domain model

| Concept | Notes |
| --- | --- |
| Workspace | One per installation. Holds settings and the signing keys. Most documents carry `workspaceId` and `projectId`. |
| Project | The single support project: name, requester acknowledgement setting, SLA, automation policy. |
| User | Role `admin` or `agent`. Status active or disabled. Optional TOTP MFA and a personal signature. |
| Group, group membership | A queue. Agents belong to several groups. A default Triage group receives tickets created without a group. |
| Contact | A requester. Unique per project by normalised email. |
| Organisation | A customer organisation, created by the integrating system, with `active`/inactive. Inactive keeps existing links but cannot be linked to new contacts or tickets. open-kase never infers an organisation. |
| Ticket | Subject, priority, status, group, assignee, requester contact, optional organisation, tags, request type, CC addresses, version, `source` (`web` or `api`). |
| Message | `public` or `agent` visibility, author kind (`user`, `integration`, `contact`), per-ticket sequence number. |
| Attachment | Bytes in GridFS, metadata in `attachments`, attached to one message. |
| Watcher | A user who gets notifications for a ticket. Self-only (`PUT/DELETE /tickets/:t/watchers/me`); watching grants no access. |
| Ticket link | Related, duplicate, follow-up, copied-from. |
| Scheduled message | A staff message sent later by a durable job. |
| Canned reply (macro), tag, hold reason, request type | Project catalogs. Tags and hold reasons are admin-managed. |
| Integration | A service principal with a hashed token and chosen scopes (section 8). |
| Webhook endpoint | URL, subscribed event types, headers, auth, TLS material, optional agent-note grant. |

### Priorities and statuses

Priorities are integers 1 to 4, shown only by name: Critical, High, Normal (default), Low. Request type is separate
from priority.

Statuses map to five lifecycle categories (`internal/statuses`): `open`, `waitingCustomer` ("Awaiting customer"),
`onHold` ("On hold"), `resolved`, `archived`. There is no "In progress": claiming or assigning keeps a ticket Open.
Custom statuses may map only to `open` or `waitingCustomer`.

- **On hold** is entered only from Open or Awaiting customer, with a managed hold reason (`holdReasons`, each with
  `maxDays`). The hold lasts exactly that long. It ends through a durable `ticket.holdExpire` job, a staff status
  change, or a requester reply. The assignee is kept.
- **Resolved** can be reopened. **Archived** is terminal and immutable through ordinary operations; staff create a
  follow-up or a copy instead.
- **Copy and follow-up.** Copy creates a new Open ticket linked `copiedFrom` that takes the requester, CC, organisation,
  priority, request type, group, tags, first public message and its attachments. Follow-up copies only subject,
  requester, request type and group, and links back. Both work on archived tickets.
- **Organisation rule.** A ticket created through an integration (`source = api`) has an organisation managed by that
  integration: a signed-in user who tries to change it gets `403 organisationManagedByIntegration`.
- **Concurrency.** Ticket mutations use an optimistic `version` sent as `If-Match`. Claim is an atomic conditional
  update, so of several concurrent claims exactly one wins.

### SLA

One SLA per project (no name, no policies list). One clock: first response, in wall-clock minutes per priority
(24/7, no working hours). The first public reply by staff satisfies it. Resolve or archive before a reply stops it.
Reopening starts no new clock. Turning SLA off stops open cycles. Warnings and breaches are produced by durable
`sla.check` and `sla.stop` jobs (`internal/sla`).

### Lifecycle automation

`internal/automation` holds the project policy (all rules off by default): follow-up reminders to the requester of
tickets awaiting them, auto-resolve after unanswered reminders, auto-archive after N days resolved. A periodic
`automation.scan` job finds due tickets and enqueues `automation.act` jobs. Each act job re-reads the ticket inside
its fenced transaction and acts only if the ticket's automation generation is unchanged, so a requester reply
cancels pending reminders atomically.

## 3. Modules

```
app.go                 package main: signal handling, exit codes, calls runtime.Execute
runtime/               composition root: bootstrap env, Container wiring, commands, workers, server, doctor, seed-demo
package/               shared, domain-free packages
  clock fault logger pagination product ratelimit secrets validation
templates/             embedded email templates (templates/email/<name>.v1.{html,txt,subject.txt})
internal/
  access/              Principal, capabilities, roles, integration scopes, ProjectScope
  auth/                login, sessions, refresh rotation, JWT keys, MFA, password reset, invitations, account
  users/ customers/ projects/ organisations/ holdreasons/ macros/ statuses/ sla/ automation/ reports/
  integrations/        API tokens (service principals)
  tickets/             the core service: create, claim, assign, status, messages, attachments, scheduled
                       messages, watchers, CC, links, copy, hold, search, queues, redaction
  outbox/              durable event types and the Record the dispatcher reads
  jobs/                durable job model and lease-based Runner
  notifications/       dispatcher (outbox -> jobs), recipient planning, email sender, delivery records
  webhooks/            destination policy, HTTP client, envelope, event types, retry schedule
    endpoints/         webhook endpoint service and deliverer
  mailer/              Mailer interface, SMTP adapter (go-mail), fake
  live/                server-sent-event hub fed from the outbox
  audit/ auditlog/     append-only audit events (write side, admin read side)
  idempotency/         Idempotency-Key records
  installation/        init: workspace, master-key check, first admin, project
  settings/            versioned settings and the immutable Snapshot
  cli/                 user commands that call a running server's REST API (login, tickets, ...)
  transport/http/      Gin REST adapter, OpenAPI document, SPA serving, SSE endpoint, readiness
  transport/mcp/       MCP adapter (official Go SDK)
  persistence/mongo/   repositories, transactions, EnsureSchema
```

Dependency direction: transports and CLI call application services; services depend on small interfaces they own
(repositories, `Mailer`, job store); `persistence/mongo` implements those interfaces. Domain packages do not import
Gin or BSON. `runtime` is the only place that wires concrete types together (`runtime/container.go`).

## 4. Request flow

```
REST (Gin)  \
MCP tools    >  authenticate -> access.ProjectScope -> service (validate, authorize, rules) -> repository
CLI (user)  /   (CLI calls REST over HTTP)             |                                       (MongoDB txn:
                                                       +-- audit event + outbox event -------   change + audit + outbox)
```

1. **Authenticate.** The bearer middleware accepts a user access token (JWT) or an integration token (`kase_live_...`).
   Both resolve to an `access.Principal` from MongoDB on every request; a JWT carries identity and session only, never
   permissions. REST and MCP share this middleware (`transport/http/mcp_mount.go` mounts `/mcp` behind it).
2. **Scope.** `access.Service.Scope` loads the project and the principal's capabilities and groups (an agent sees the
   queues of their groups plus tickets assigned to them; an admin sees all; an integration sees what its scopes allow).
3. **Service.** The service validates input with `package/validation` (field-rule catalog, stable error codes; unknown
   fields are rejected), checks capabilities and ticket state rules, and calls the repository.
4. **Repository.** Business mutations run in one MongoDB transaction (snapshot reads, majority writes) that commits the
   change, its audit event and its outbox event together.
5. **Idempotency.** Create routes accept `Idempotency-Key`: the key is scoped to principal and route, a retry with the
   same body replays the stored response, a different body gets `409 idempotencyKeyReused`. The MCP adapter imports the same idempotency store.
6. **Errors.** `package/fault` kinds map to RFC 9457 problem responses with `urn:open-kase:error:<kind>` types.
7. **Limits.** In-process token buckets per principal (authenticated routes) or client IP (`/auth`), in
   `transport/http/ratelimit.go`: auth 60/min, ticket create 120/min, message post 120/min, upload 60/min, search
   120/min, connection test 6/min. They do not use Redis. Login, MFA and reset failures are additionally throttled per
   account and IP in MongoDB (`loginThrottle`).

REST routes live under `/api/v1`; `/api/v1/openapi.json` serves the OpenAPI document generated from
`transport/http/openapi_ops.go` (`open-kase openapi --out docs/openapi.json` writes it). Health: `/health/live` and
`/health/ready` (MongoDB, master key, settings are required checks; Redis is optional).

External calls (SMTP, webhooks) and GridFS byte transfers never happen inside a database transaction.

### CLI commands

Operator commands (`runtime/commands.go`): `serve`, `init`, `doctor`, `version`, `health`, `seed-demo` (refused in
production), `worker`, `openapi`, `jobs list|retry`, `keys rotate-jwt`. User commands (`internal/cli`) sign in to a
running server and call REST; see `docs/cli.md`.

## 5. Outbox, jobs, workers and leases

**Outbox.** The service writes an `outbox` document (event type, ticket and message IDs, audience, actor, ticket
version, a ticket snapshot) in the same transaction as the change. Events carry identifiers and the audience, never
message bodies; workers load the current authorized projection when they run. Event types are listed in
`internal/outbox`. Demo-seed events are marked suppressed.

**Dispatcher** (`notifications.Dispatcher`). It claims one pending outbox event with a lease (`leaseUntil`, fencing
generation), plans recipients and endpoints, and creates the jobs and delivery records in one fan-out step. Job keys
are unique (`email:<event>:<kind>:<id>`, `webhook:<event>:<endpoint>`), so repeating a dispatch creates nothing new.
A planning failure retries with growing delay and marks the event failed after 10 attempts.

**Jobs** (`internal/jobs`, collection `jobs`). Durable work with a unique `key`. States: `queued`, `running`,
`succeeded`, `dead`. A runner takes a job with an atomic conditional update that sets `leaseOwner`, `leaseUntil` and
increments `generation`; every later write (retry, completion, dead-letter) must match owner and generation, so a
worker whose lease expired cannot commit stale results. The lease is 2 minutes, renewed every third of that; polling
is 2 s. Retries use 1 min, 5 min, 30 min, 2 h, 12 h with jitter (6 attempts total), then `dead`; `open-kase jobs retry`
requeues one. Jobs carry identifiers and small constrained payloads only. Delivery is at least once.

Job kinds registered in `runtime/workers.go`: `email`, `email.test`, `email.account`, `webhook`, release-assignment
(when a user is disabled or leaves a group), `sla.check`, `sla.stop`, `automation.scan`, `automation.act`,
`ticket.scheduledSend`, `ticket.holdExpire`, and deletion of removed attachment files.

**Workers.** `serve` runs the dispatcher and a pool of job workers in-process (`--worker-concurrency`, default 4,
1 to 64; `--workers=false` disables them). `open-kase worker` runs only the dispatcher and workers, with no HTTP.
Leases and fencing make extra processes safe, but no deployment profile relies on that. On shutdown no new job is
taken, attempts in progress get a 15 s grace period, then are cancelled and recorded as failed attempts. `serve`
also runs: a settings refresh every 5 s, a 15-minute sweep (expired staged uploads, expired holds, automation
scheduling), and the Redis live-hint listener.

**Redis** is a latency optimisation only: pub/sub wake-ups for the job runner and change hints for the live hub. It
is never the record of jobs, sessions or state. With it down, work is delayed by at most the poll interval.
`serve`, `worker` and `doctor` require `REDIS_URI` to be set.

**Live updates.** `GET /api/v1/events` is a server-sent-event stream. The `live.Hub` polls recent outbox records for
projects with subscribers; each stream authorizes every change for its own principal before sending
`{type, ticketId, version}`. Streams are invalidation hints; clients refetch after reconnect.

## 6. Notification pipeline

1. A change commits an outbox event.
2. The dispatcher calls `PlanEmail` (`notifications/recipients.go`) and the webhook planner.
3. Each email target or endpoint gets a `notificationDeliveries` record (status pending) and a job.
4. The job handler re-checks access, account status and preferences, renders a template from `templates/email`, and
   sends. A delivery moves through `pending`, `retrying`, `sent`, `failed`, `unknownOutcome`, `skipped` or `skippedUnconfigured`; a dead-lettered job marks its delivery failed.

**Email recipients.** The author of a change never receives their own notification, and a person is planned once.

- Ticket created: the requester acknowledgement (project setting) and the group's agents.
- Public reply by staff: requester (plus CC addresses), assignee, watchers.
- Public reply by the requester: assignee and watchers; the group queue if unassigned.
- Agent note: a link-only notice to the assignee and staff watchers; never to contacts or CC addresses.
- Claim, release, assignment: the new assignee, or the group when unassigned.
- Resolve or archive: requester; resolve, reopen, archive: assignee and watchers.
- Follow-up reminder: requester. Scheduled message held for review or cancelled: its author.
- SLA warning and breach: assignee and group (staff only). Hold ended: the assignee only.
- Every email to the requester is repeated, one delivery each, to the ticket's CC addresses.
- Watcher and group-queue emails can be muted in preferences; requester, assignee and core notices cannot.

Requester emails carry no agent link. If `notifications.requesterLinkBaseUrl` is set, they link to
`<base>/<ticket number>` in the integrating system; otherwise they ask the requester to use the system they wrote from.

**SMTP** (`internal/mailer`, `github.com/wneessen/go-mail`). Settings: host, port, username, password (encrypted),
from address and name, TLS mode `starttls`, `tls` or `none` (authenticated SMTP requires TLS), optional custom CA
certificate, and `tlsSkipVerify` (weakening verification requires re-entering the password). SMTP cannot promise
exactly-once delivery; an uncertain outcome is recorded as `unknownOutcome`.

**Webhooks** (`internal/webhooks`, `docs/webhooks.md`). Events: `ticket.created`, `ticket.updated`,
`ticket.message.public`, `ticket.message.agentNote` (only for endpoints with the agent-note grant), `ticket.assigned`,
`ticket.statusChanged`, `ticket.slaWarning`, `ticket.slaBreached`, `webhook.test`. Requests are unsigned. Each endpoint
has custom headers, auth `None`, `Basic` or `Token`, and an optional custom CA and client certificate. Credentials
and keys are encrypted and write-only. Retries run 1 + the schedule in `webhooks/retry.go`. SSRF policy
(`webhooks/client.go`): private networks are allowed (admins configure endpoints); link-local, unspecified, reserved
and cloud-metadata addresses are never allowed, checked again at dial time; loopback and plain HTTP only when
`APP_ENV=development`.

## 7. Privacy

Agent notes must never reach a requester, a requester-side token, an email to a contact, or a public webhook.

- Message queries filter by `visibility` in the repository, before
  pagination, counts, search snippets and exports. `tickets.Service` computes the audiences a principal may read:
  `public` only, or `public` and `agent` for users and tokens with `tickets:internal` or `tickets:all`.
- Tokens without a staff scope get the requester projection of a ticket (no assignee, tags, SLA, version, notes).
- Full-text search matches message bodies by audience (the text index is prefixed by `projectId` and `visibility`)
  and ticket subjects.
- Outbox events, jobs and delivery records hold IDs and the audience, not bodies. Webhook agent-note events need the
  explicit endpoint grant. Audit events and logs hold field names and IDs, never secrets or private bodies.
- Watching grants no access; access is rechecked when an email or webhook is sent and on every SSE change.
- Signatures: each user may have a personal signature, and settings hold a workspace signature. The server appends
  both to staff public replies (not to agent notes or requester replies); a request can opt out with
  `addSignature=false`.
- Admin-only message redaction replaces a message body and removes its attachments; it is audited.

These are application-level rules, not encryption from the installation operator, who has database access.

## 8. Authentication

- **Users.** Password (Argon2id) login at `POST /api/v1/auth/login`. Response: a short-lived EdDSA JWT access token
  (default 10 min) kept in browser memory. The refresh token is opaque, rotating, and set as an HttpOnly,
  `SameSite=Strict`, path-limited cookie (default 14 days); a CSRF cookie plus `X-CSRF-Token` header and an Origin
  check protect `refresh` and `logout`. Refresh reuse is detected, with a 10 s grace window for two tabs refreshing
  at once. Cookies are `Secure` except when `APP_ENV=development`.
- **JWT.** Only EdDSA is accepted. Issuer is per workspace, audience `open-kase-api`. Each request checks the session
  and the user's active status in MongoDB. Signing keys are stored encrypted (`signingKeys`); `keys rotate-jwt` adds a
  key and old ones verify until their tokens expire.
- **MFA.** TOTP with recovery codes. Login with a second factor is a two-step flow (`/auth/mfa`). Setting
  `auth.requireAdminMfa` forces admins to enrol before they can use anything but the enrolment routes. Disabling MFA and replacing recovery
  codes need the password plus a current TOTP code or an unused recovery code.
- **Accounts.** Invitations and password reset use hashed single-use tokens (`accountTokens`), delivered by email.
  The last active admin cannot be disabled or demoted.
- **API tokens** (`internal/integrations`). One kind. The creator picks scopes: `tickets:read`, `tickets:create`,
  `tickets:replyAsRequester`, `tickets:comment`, `tickets:internal`, `tickets:write`, `tickets:all`, `contacts:read`,
  `contacts:write`, `tags:read`, `tags:write`, `holdReasons:read|write`, `organisations:read|write`. The token
  `kase_live_<random>` is shown once; only its SHA-256 hash is stored. A token holding `tickets:comment`,
  `tickets:internal`, `tickets:write` or `tickets:all` acts as staff (a bot under its own name); others act for the
  requester side. No scope allows claiming or administration, and no scope exceeds what an agent may do. Every token
  write is audited with the scopes that allowed it.
- **MCP.** `/mcp` (Streamable HTTP, stateless, official Go SDK) authenticates with the same bearer middleware and
  calls the same services, so scopes behave identically. It checks `Origin` against the configured base URL
  (DNS-rebinding protection), rate limits per credential, and has a 30 s call timeout. See `docs/mcp.md`.

## 9. Data and indexes

All state is in MongoDB; attachments use a GridFS bucket (`attachments.files`, `attachments.chunks`). MongoDB must be
a replica set because mutations use transactions; a single-node replica set is the supported setup.

**`EnsureSchema`** (`persistence/mongo/schema.go`) is the only schema mechanism. There are no migrations. It runs at
`init`, `serve` and `worker` start, after the replica-set check, and is idempotent: it creates missing collections,
re-applies each collection's JSON-schema validator with `collMod` (strict, action error), and runs `createIndexes`.
Changing a collection's shape during development means wiping the database and running `init` again.

Collections: `workspaces`, `settings`, `users`, `userMfa`, `mfaChallenges`, `accountTokens`, `sessions`,
`refreshTokens`, `signingKeys`, `loginThrottle`, `servicePrincipals`, `apiCredentials`, `projects`, `groups`,
`groupMemberships`, `statuses`, `requestTypes`, `contacts`, `organisations`, `idempotencyRecords`, `tickets`,
`messages`, `drafts`, `scheduledMessages`, `ticketWatchers`, `ticketLinks`, `tags`, `attachments`, `counters`,
`outbox`, `jobs`, `notificationDeliveries`, `webhookEndpoints`, `slaPolicies`, `slaInstances`, `automationPolicies`,
`macros`, `holdReasons`, `auditEvents`.

Notable indexes (see the file for the full list):

- `tickets`: unique `(projectId, number)`; queue indexes `groupQueue` and `assigneeQueue`; `(projectId, statusCategory,
  updatedAt)` and `(projectId, updatedAt)` for lists; SLA, tag, organisation, hold-reason and automation indexes; a
  text index on `subject` prefixed by `projectId`.
- `messages`: unique `(ticketId, seq)`; `(projectId, ticketId, visibility, createdAt)`; a text index on `body` prefixed
  by `projectId` and `visibility`.
- `jobs`: unique `key`; `(state, runAt)` and `(state, leaseUntil)` for acquisition and lease expiry. `outbox`:
  `(state, createdAt)` and `(state, leaseUntil, createdAt)`.
- Uniques: contact `(projectId, emailNormalized)`, user `(workspaceId, emailNormalized)`, group, tag, hold reason and
  organisation names per project, `ticketWatchers (ticketId, userId)`, `ticketLinks`, and every token hash.
- TTL indexes: `idempotencyRecords`, `refreshTokens`, `sessions`, `accountTokens`, `mfaChallenges`, `loginThrottle`.

Ticket numbers come from an atomic per-project counter in `counters`. Messages, watchers, audit history and
attachments live in their own collections, never embedded in the ticket.

### Sizing and limits

Operator guidance that still holds (from measurements on a 4 vCPU, 8 GiB host with about 20,000 tickets and 200,000
messages; indicative, not guarantees):

- Every ticket and message query used an index. Ticket reads by number use `(projectId, number)`.
- **Full-text search is the costliest operation.** MongoDB `$text` scores every message containing the term, so a
  common word in a large deployment examines tens of thousands of messages (hundreds of ms of server time). Search
  is rate limited per principal (120/min) and capped at `MaxSearchCandidates` (1000) candidates per audience; totals
  beyond the cap are reported as capped. Very large deployments would need a dedicated search engine.
- **The queue subject filter (`?q=`) is a substring match** that cannot use an index; it reads visible tickets in
  update order and stops at a full page, so rare words on huge queues are the slow case.
- **Queue counters** run one count per sidebar view.
- **Claims are transactions.** A claim race had exactly one winner every time; tail latency rose under many concurrent
  claimers.
- **Memory:** size MongoDB so data and indexes of the active tickets fit in the WiredTiger cache.
- **Pagination:** default 25, maximum 100. Attachment limits are settings (defaults: 10 MiB per file, 5 files and
  50 MiB per message, 100,000 characters per message).
- One API process serves everything; rate limits and live-update fan-out are per process.

## 10. Configuration

- **Bootstrap** comes from the environment or flags (`runtime/bootstrap.go`): `APP_ENV` (`production` default,
  `development`, `test`), `MONGODB_URI`, `REDIS_URI`, `OPEN_KASE_MASTER_KEY_FILE` (default `/etc/open-kase/master.key`),
  `OPEN_KASE_HTTP_ADDR` (`:8080`), `OPEN_KASE_LOG_LEVEL`, `OPEN_KASE_LOG_FORMAT` (JSON in production), 
  `OPEN_KASE_TRUSTED_PROXIES` (proxies whose `X-Forwarded-For` is trusted), `OPEN_KASE_UI_DIR` (directory of the built
  SPA to serve). The database name defaults to `open_kase`.
- **Product settings** live in MongoDB (`settings`, one document per workspace, versioned): product name, description,
  base URL, workspace signature, token lifetimes, `requireAdminMfa`, file limits, SMTP, and the requester link base
  URL. `settings.Service` validates them with `package/validation` on load and on update and publishes an immutable
  `Snapshot`; each process refreshes it every 5 s and keeps the last valid snapshot if a refresh fails. Per-project
  settings (requester acknowledgement, SLA, automation) are in their own collections.
- **Master key.** A 32-byte key in a file outside MongoDB, created by `init` and loaded at start by
  `secrets.LoadKeyFile`. `package/secrets` encrypts database-held credentials (AES-256-GCM) with a context string per
  field: the SMTP password, webhook credentials and client keys, the JWT private keys, TOTP secrets. Back it up
  separately from the database; without it these values cannot be decrypted. Do not store it beside the dump.
- **Logging.** One injected `log/slog` logger. Request logs carry a request ID; audit events are separate and
  append-only.

## 11. UI structure

`ui/` is a Vue 3 + TypeScript + Vite single-page app with Vue Router, Pinia, Tailwind CSS, Tabler icons, IBM Plex
fonts, a TipTap-based Markdown editor, and markdown-it with DOMPurify for rendering (raw HTML disabled). Themes:
System, Light, Dark. The design direction is "Ledger". There are no frontend tests (owner decision); the API has the
test suite.

```
ui/src/
  main.ts App.vue router/index.ts      routes, auth guard (public routes: sign-in, reset, invitation)
  api/                                 client.ts (fetch wrapper, in-memory access token, refresh), endpoints.ts,
                                       types.ts, errors.ts, schema.d.ts (generated from docs/openapi.json)
  stores/                              session (user, sign-in, MFA, refresh), workspace (project, groups, catalogs),
                                       preferences, brand
  layouts/                             AppShell (agent work area), AdminShell, AccountShell, AuthFrame, ShellFrame
  views/                               QueueView, TicketView, NewTicketView, SignInView, NotFoundView
    auth/                              ForgotPassword, ResetPassword, AcceptInvitation
    account/SecurityView               password and MFA (the signature is in PreferencesDialog)
    admin/                             Users, User, Groups, SLA, Canned replies, Tags, Hold reasons, Organisations,
                                       Integrations, Webhooks, Logs, Reports, Configuration
  components/
    base/                              reusable inputs, buttons, modals, tables, badges, MarkdownEditor/View
    ticket/                            timeline, composer helpers, hold, copy, follow-up, schedule, watchers, links, tags
    notifications/ logs/ project/ live/ account/   delivery log, webhook form, audit log, automation, live indicator
    ReplyComposer, TicketTable, QueueNav, GlobalSearch, MessageItem, TicketSidePanel, ...
  composables/                         useQuery, usePagedQuery, useLiveUpdates (SSE), useToasts, useStagedAttachments, ...
  queues.ts                            default views: Assigned to me, Unassigned, Watching, Awaiting customer, On hold,
                                       Due soon, Resolved, Archived (plus All)
```

Behaviour worth knowing:

- The access token stays in memory; the refresh cookie is HttpOnly. A refresh runs once for concurrent 401s.
- The composer shows the message audience (public reply or agent note) beside the editor; it is never inferred.
  Drafts are kept and a newer reply shows a conflict instead of silently sending.
- Live updates arrive as SSE invalidation hints; the UI refetches the affected ticket or queue.
- "Ago" dates reveal the full date on click. The UI detects a stale build after a deploy and shows a banner.
- The Go binary serves the built SPA from `OPEN_KASE_UI_DIR` with a fallback to `index.html` for client routes
  (never for `/api`, `/mcp` or `/health`) and its own Content-Security-Policy; API responses use a strict
  `default-src 'none'` policy.

## 12. Deployment

The release artifact is one container image, `ghcr.io/arksnorman/open-kase`, built from the root `Dockerfile`. It
contains the `open-kase` binary and the built SPA; the container runs `serve --ui-dir /srv/ui`, so one process serves
the API, MCP and the UI. `docs/install.md` gives a Docker Compose file with three services (MongoDB as a single-node
replica set, Redis, and `open-kase`) and the first-run steps (`init`, `doctor`), plus a native-binary install and
reverse proxy notes. Day-two tasks are in `docs/operations.md`. This is a single-host setup and is not highly
available; nothing here describes a multi-replica profile.
