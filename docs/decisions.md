# open-kase: decisions

The decisions that currently hold, grouped by area, each with a short reason. This is the first release (v0.1.0),
so superseded choices and their history are not kept here. Behaviour details live in
[`api-contract-v1.md`](api-contract-v1.md) and `openapi.json`; the design lives in `architecture.md`.

## Product scope

| Decision | Reason |
| --- | --- |
| The product is **open-kase** (repository `github.com/arksnorman/open-kase`, binary `open-kase`, environment prefix `OPEN_KASE_`, database `open_kase`). The name is defined once in `api/package/product/product.go` | One place to rename; no scattered literals |
| Code lives in `api/` (Go module `github.com/arksnorman/open-kase/api`) and `ui/` (Vue). `docs/` stays at the repository root (install and operations docs live there; there is no `deploy/` directory and no helper scripts). `app.go` only starts `runtime`; there is no `cmd/` and shared packages live under `package/` | Keeps the entry point thin and the layout predictable |
| One installation has one workspace and **one project**, created by `init`. No project switcher, key or project-scoped role | The simplest model that serves one support desk |
| Roles are **admin** and **agent**. Groups are team queues; there is no group-lead role | Fewer permission combinations to reason about |
| **No customer portal and no customer logins.** Only agents and admins sign in. Requesters are contacts; external systems use the API with integration tokens | Customer-facing systems already exist; open-kase is the back office |
| Message audiences are `public` (the requester) and `agent` (staff only) | Two audiences are enough and impossible to confuse |
| **No built-in AI.** External AI agents use the REST API and MCP with an API token | Keeps the product small and provider-neutral |
| Notifications are email (SMTP) and outbound webhooks only | No chat, SMS or push infrastructure to run |
| No HA or scaling work: MongoDB (single-node replica set, needed for transactions), Redis, the API and the UI. A single host is not described as highly available | Owner scope; honest about what Docker on one host gives |
| Kept features: groups, watchers, scheduled send, tags, canned replies, SLA, lifecycle automation, reports, redaction, MFA, integrations, REST API, MCP | The working core of a help desk |
| Tags are an admin-managed catalog; tickets can only carry catalog tags | Consistent labels, no tag sprawl |
| A single host is the design: Docker Compose on one machine. HA and multi-node scaling matter for multi-tenant services, which this is not | Keeps operations simple |
| Removed features: exports, custom fields, saved views, macro actions, per-project settings | Not needed for the simplest version that works |
| Pre-release: **no migrations and no compatibility shims**. One idempotent `EnsureSchema` (`api/internal/persistence/mongo/schema.go`) creates collections, validators and indexes at `init`, `serve` and `worker` start. When a collection shape changes, wipe the development database and re-run `init` and `seed-demo` | Nothing is deployed, so migration code would be dead weight |
| Everything on the wire is camelCase: JSON fields, query parameters, enum values, error codes, audit actions, webhook event types, MCP tool and argument names, scope names, MongoDB fields | One convention end to end |
| Required stack: Go with Gin and the official MongoDB driver; Vue 3, TypeScript, Vite, Vue Router, Pinia, Tailwind; versioned REST under `/api/v1` with a generated OpenAPI 3.1 document; no GraphQL | Boring, well-supported choices |
| Dependencies are pinned (`api/go.sum`, `ui/package-lock.json`, container image tags); email uses `github.com/wneessen/go-mail` behind a `Mailer` interface. See `dependencies.md` | Reproducible builds; a maintained SMTP library |
| Modular monolith: transport adapters (REST, MCP, CLI) call application services, which use domain rules and consumer-owned repository interfaces. All transports share authorization, validation, state transitions, audit and idempotency | One behaviour regardless of the entry point |
| **No changelog and no migrations** while pre-release; `EnsureSchema` is the only schema step (see above). Git history starts fresh at v0.1.0 | Nothing deployed needs an upgrade path |
| Tests cover the API only (real MongoDB replica set); there are no frontend tests | Owner scope |

## Tickets and statuses

| Decision | Reason |
| --- | --- |
| Tickets are identified and shown by number: `#1, #2, ...`. `/tickets/{t}` accepts the number, `#number` or the ID. The number comes from a counter incremented in the create transaction | Short, human, unique |
| Priorities are integers 1-4, shown only by name: Critical, High, Normal (default), Low. Request type is separate from priority | Stable API values, readable UI |
| Built-in statuses: **Open**, **Awaiting customer**, **On hold**, **Resolved**, **Archived**. There is no "In progress": claiming or assigning keeps a ticket Open | Assignment already says who is working on it |
| **Archived is terminal and immutable.** Changes answer `409 ticketArchived` with a "create follow-up" action; Resolved can be reopened. **Tickets are never deleted** | Auditable history; a follow-up keeps the link without rewriting the past |
| A ticket has exactly one current group. New tickets enter a group (default **Triage**, which may be renamed but not deactivated or deleted) unassigned. **Claiming keeps the group** and is atomic: one concurrent claim wins, others get `409 ticketAlreadyClaimed` | Queues stay meaningful; no double ownership |
| The assignee must be an active member of the ticket's group. Transfers keep an eligible assignee and clear an ineligible one; removing or disabling a user releases their ineligible tickets through a background job | Assignments never point at someone who cannot work the ticket |
| **On hold** is entered only from Open or Awaiting customer, with a managed **hold reason** (`maxDays` 1-365). It lasts exactly `maxDays`, then a durable job returns the ticket to Open (assignee kept). A staff status change or a requester reply ends it earlier. The requester is never notified; the SLA keeps running | Makes waiting on someone else explicit and bounded |
| Hold reasons are admin-managed (up to 100, names unique); a new `maxDays` affects only later holds; deleting a reason in use is refused; past holds keep the reason name | No lost history, no half-deleted state |
| A requester reply reopens an Awaiting customer, On hold or Resolved ticket to Open and ends a hold | The requester has the ball back with staff |
| Agent notes never change status, `lastPublicMessageId` or public activity | Internal chatter must not leak into the requester timeline |
| Visibility: admins see every ticket; agents see their groups' tickets plus open, awaiting-customer and on-hold tickets assigned to them. Authorization happens before pagination, counts, search snippets and notifications | Private content never leaks through totals or previews |
| Mutations use optimistic `version` with `If-Match` (`428` when missing, `409 versionConflict` when stale). Message posts use `expectedLastPublicId` so a stale reply is never sent silently | No silent overwrites |
| The subject is locked after creation for staff (`403 subjectLocked`); only the API token that created the ticket may change it | The requester's wording stays as written |
| Releasing a ticket to its group queue requires a reason (3-500 characters), recorded in the audit and activity | Shows why work changed hands |
| `ccEmails` (up to 10) are copied on every requester email; staff can edit them | Matches how requesters involve others |
| Follow-up creates a linked ticket (works on archived tickets); copy creates a new Open ticket with the original's requester, CC, organisation, priority, request type, group, tags and first public message and attachments (bytes duplicated), authored by the copier. Links: `relatedTo`, `duplicateOf`, `followUpTo`, `copiedFrom` | Provenance is the link, never a fake requester message |
| Redaction replaces a message body with `[redacted]` (admin; optional attachment removal with durable byte deletion). There is no message delete or edit | Compliance needs without rewriting history |
| Reply signatures: an optional personal signature (user preference) and workspace signature (`product.signature`), up to 1000 characters each, appended **server-side** to public replies written by staff users only, with a per-reply opt-out (`addSignature: false`). Never on notes, requester messages or token posts | Consistent sign-offs without cluttering the composer |
| Scheduled send: a durable job sends the message at `sendAt` after re-authorizing the author and re-checking the ticket, at most once. A newer requester reply or an invalid status change moves it to `needsReview`; an archived ticket cancels it | Never sends a stale reply unseen |
| Drafts are private to their author per audience and ticket; a newer reply is shown as a conflict, never overwritten | Preserves work without sending stale text |
| Watchers receive updates but gain no access; each watcher's access is re-checked when listed or notified | Watching is not a permission |
| Attachments are staged (24 hours), then attached to a message; bytes live in GridFS outside any transaction; downloads are authorized per message and served with `nosniff` and `no-store` | Shared storage, no path traversal or leakage |
| Full-text search uses MongoDB text indexes with one query per audience the caller may read | No extra search infrastructure; no private-note leakage |

## SLA and automation

| Decision | Reason |
| --- | --- |
| **One SLA**, no name, **first response only**, counted in wall-clock time **24/7**; no working hours or holidays. Integrating systems can apply their own calendars | The simplest model that stays honest |
| The SLA is off until switched on. Each ticket cycle snapshots the SLA version it started with; changes apply to new tickets only. Targets are per priority (`firstResponseMinutes`); the warning point is `warningPercent` (default 80) | Predictable, not retroactive |
| The clock starts at creation and is met only by the first public reply by staff (a user or a `tickets:comment` token). Notes, claims and requester replies never count. Resolving or archiving first stops it unmet. **Reopening starts nothing** | Matches what customers experience as "first response" |
| Warnings and breaches are durable timer jobs emitting `ticket.slaWarning` and `ticket.slaBreached` once per deadline; email goes to the assignee and group members, never the requester; both are webhook events | Restart-safe alerts |
| Turning the SLA off closes open cycles in a background job and announces nothing afterwards | No stale alerts |
| Lifecycle automation (admin, off by default): follow-up reminders to the requester while Awaiting customer, auto-resolve after the last reminder (always with notice), auto-archive of resolved tickets. Durable scan and per-ticket jobs re-check before acting; a requester reply or status change resets the clock | Housekeeping without surprises |
| Reports: a bounded live traversal (at most 92 days and 10,000 samples) for admins, not a stored snapshot | Honest about cost and accuracy |

## Notifications and email

| Decision | Reason |
| --- | --- |
| Requester emails never contain message text: a "response received" notice with an optional link (`notifications.requesterLinkBaseUrl`); replies by email are not processed. Staff emails link to the SPA; agent notes notify with a link only | Email is an unsafe channel for private content |
| Recipients are chosen by event (requester, assignee, watchers, group queue while unassigned); authors never get their own notice; each recipient's access is re-checked right before sending; watcher and queue updates can be muted, assignee and requester notices cannot | Relevant, permission-safe mail |
| Delivery is **at least once**. Each email has one recipient and a stable `Message-ID`; an outcome that cannot be known (SMTP may have accepted it) is `unknownOutcome` and not retried automatically | SMTP cannot promise exactly once; say so |
| Retries use 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours with 20% jitter, then dead-letter for manual retry | Bounded, jittered backoff |
| SMTP settings live in MongoDB, with the password encrypted. TLS modes `starttls` (default), `tls` and `none` (local relay only, no credentials). Server certificates are verified by default; an optional custom CA and an explicit skip-verify (not recommended) exist. Weakening the destination requires re-entering the password | Works with self-signed relays without silent downgrades |
| All business mutations commit together with their audit event and an outbox event; a dispatcher plans durable jobs from the outbox. External calls (SMTP, webhooks) never happen inside a database transaction | No lost or phantom notifications |
| Jobs are leased in MongoDB with fencing so a stale worker cannot commit. Redis only wakes workers sooner and is never the record of jobs or sessions | Correctness does not depend on Redis |

## Webhooks

| Decision | Reason |
| --- | --- |
| Webhooks are **unsigned**. Authenticity comes from HTTPS plus per-endpoint authentication: None, Basic or Token, up to 20 custom headers, an optional custom CA and an optional client certificate (mutual TLS) | Standard mechanisms receivers already support |
| Credentials, header values and the client key are encrypted with the master key and **write-only** (responses report `...Configured` flags) | Secrets are never readable after saving |
| Each endpoint reads through its own read-only principal with the public view. Agent-note events are sent only when an admin enabled `includeAgentNotes` | Least privilege; private notes opt-in |
| Payloads are built at the first attempt from the event snapshot, stored, and retried byte-identical; redaction rebuilds unsent payloads | Retries are safe; redaction is honoured |
| Event types: `ticket.created`, `ticket.updated`, `ticket.message.public`, `ticket.message.agentNote`, `ticket.assigned`, `ticket.statusChanged`, `ticket.slaWarning`, `ticket.slaBreached`, `ticket.followupReminder`, plus `webhook.test` | Covers the ticket lifecycle; reminders reach integrations that email requesters themselves |
| Admin switch `notifications.requesterEmails` (default on) suppresses all email to contacts and CC addresses; each is recorded as a skipped delivery `requesterEmailsOff` | Installations that notify requesters through webhooks only |
| Private-network targets are allowed (admins configure webhooks, LAN receivers need no setting); loopback (development only), link-local, multicast, reserved ranges and cloud metadata addresses are always refused, checked at connect time. No allowlist. HTTPS only outside development; no credentials in the URL; redirects are not followed; environment proxies are not used | SSRF protection without configuration |
| A webhook's stored secrets (Basic password, Token, secret header values) are never sent to a new destination: a `PATCH` that changes the scheme, host or port, or sets a different non-empty custom CA, must enter them again (`422`, field `auth` or `headers[i].value`, code `required`). Setting authentication to None needs no re-entry; path-only changes need none; the client key is not sent to the receiver and is unaffected (same rule as the SMTP password) | A stolen or careless admin session could otherwise redirect a webhook and receive its credentials |

## Integrations and tokens

| Decision | Reason |
| --- | --- |
| **One kind of API token**; the creator picks the scopes: `tickets:read\|create\|replyAsRequester\|comment\|internal\|write\|all`, `contacts:read\|write`, `tags:read\|write`, `holdReasons:read\|write`, `organisations:read\|write`. `tickets:all` excludes `replyAsRequester` and the non-ticket scopes | A customer-facing system and an AI agent differ only by scopes |
| A token with a staff scope (`comment`, `internal`, `write`, `all`) gets the staff view and acts as a bot under its own name; other tokens see the requester-side view (public messages only, `version` 0) | Staff data never reaches requester-side tokens |
| Tokens never exceed their scopes or what a human agent may do (no claim, release, drafts, redaction, reports or administration). With `tickets:write` they may transfer and assign tickets; a bot is never an assignee | Bounded automation |
| Tokens are `kase_live_...`, shown once, stored as SHA-256 hashes, revoked at once. **Every token write is audited** with the token and the scope that allowed it | Traceable machine access |
| MCP at `/mcp` (Streamable HTTP, stateless, official Go SDK) uses the same tokens, scopes, services and audit as REST; there is no separate MCP scope. Reply and note tools require an explicit audience; there are no ticket delete, redaction or settings tools; no OAuth | One permission model, no hidden powers |
| REST and MCP mutations accept idempotency keys (24 hours, scoped to caller and route); REST ticket and message posts replay the stored response | Safe retries for integrations |
| Browser access tokens live in memory; the refresh token is a rotating opaque HttpOnly cookie with CSRF double-submit and an Origin check; a refresh token reused after 10 seconds revokes the session. Access tokens are short-lived EdDSA JWTs checked against the live session and user on every request | Limits theft impact, tolerates several tabs |
| Passwords use Argon2id. Login throttling is per account, per client and per IP; failures are identical for unknown and known accounts. TOTP MFA with recovery codes; admin MFA is required by default outside development and test. Turning MFA off and replacing recovery codes need the password **and** a current TOTP code (each time step once) or an unused recovery code; wrong codes are throttled per user | A stolen session or a known password alone cannot remove the second factor |
| Product settings live in MongoDB as a validated, versioned snapshot; database-held credentials are encrypted with a master key kept in a separate secret file. JWT signing keys persist and can be rotated with `open-kase keys rotate-jwt` | Secrets never sit beside their key |

## Organisations

| Decision | Reason |
| --- | --- |
| Customer organisations (name, optional `externalRef`, active flag) are separate records. An integrating system creates the organisation, then links its ID to contacts and tickets. **open-kase never infers an organisation** | The integration owns the mapping |
| Organisations are managed by admins or tokens with `organisations:write`; they are deactivated rather than deleted while referenced | Keeps history intact |
| Signed-in users cannot change the organisation of a ticket created through the API or MCP (`403 organisationManagedByIntegration`); tokens with `tickets:write` can | The integrating system owns it |

## UI

| Decision | Reason |
| --- | --- |
| Design direction **Ledger**: neutral ink, surface and border tokens, text labels, themes System, Light and Dark, reduced motion respected | Calm, accessible and readable |
| Priority and status badges use semantic colours with distinct dot shapes, so colour is never the only cue. SLA met is green, missed red | Fast scanning without relying on colour alone |
| Default queues: Assigned to me, Unassigned, Watching, Awaiting customer, On hold, Due soon, Resolved, Archived (fixed; no saved views) | A small, learnable set |
| The composer shows the message audience explicitly beside the box and never infers it; replies and notes use Markdown (sanitized, raw HTML disabled) | Prevents sending a note to the requester |
| The ticket page has a slim header, a details panel for editing metadata inline, "Take it" to claim (keyboard `T`) and a watchers popover | Most work happens on one screen |
| "Time ago" dates reveal the full date on click; reusable base components; selects that can grow without bound are searchable | Consistent, scalable controls |
| The binary serves the built UI itself (`--ui-dir` / `OPEN_KASE_UI_DIR`; the Docker image ships it in `/srv/ui`); there is no nginx | One process to run and expose |

## Operations

| Decision | Reason |
| --- | --- |
| `.env` stays small: `APP_ENV`, `MONGODB_URI`, `REDIS_URI`, plus `OPEN_KASE_*` infrastructure flags and the master key file path | Product settings belong in the database |
| One binary with operator commands (`init`, `serve`, `worker`, `doctor`, `jobs`, `keys`, `openapi`, `health`, `version`, `seed-demo`) and user commands (`login`, `logout`, `tickets`, `settings`). See `cli.md` | One artifact to ship |
| `serve` runs the HTTP API, the outbox dispatcher and a bounded job pool; `worker` runs only background work against the same database | Simple by default, splittable later |
| `seed-demo` creates demo data with suppressed notifications and refuses to run in production unless forced | Safe demos |
| Readiness requires a writable MongoDB primary, a matching master key and loaded settings; Redis is optional (`degraded`) | Honest health reporting |
| CI runs before any image is pushed: `release.yml` calls `ci.yml` (`workflow_call`) and builds `:edge` (main) or the version tags (`v*`) only after it passed. The Compose example pins `:0.1.1` (bump on upgrade) and gives Redis a password from a secret file, carried in `REDIS_URI` as `redis://:PASSWORD@redis:6379/0` | No untested images; repeatable installs; Redis is not open to other containers without a password |
| Delivery: a single root `Dockerfile` and the Docker image `ghcr.io/arksnorman/open-kase` on GHCR, run with the Compose setup in `docs/install.md`; see `docs/operations.md` for running it | One artifact, one documented install |
| Audit events are append-only, separate from diagnostic logs, and record field names and IDs only, never message bodies or secrets. One injected `log/slog` logger, JSON in production | Safe, searchable records |
| Rate limits are per principal in process (per IP on `/auth/*`); login and MFA throttles live in MongoDB | No Redis dependency for safety |
| The Compose setup bundles a TLS reverse proxy chosen with `COMPOSE_PROFILES=nginx` or `caddy` (default nginx in `.env`; `.env.example` lists the variables). The root `compose.yaml` is the single source (the docs only summarise it); the proxy configuration is inline in it (`configs: content:`, Compose 2.23+), so one file plus `.env.example` is all a user copies. The proxy only reverse-proxies to `open-kase:8080` (the app serves the UI and its CSP headers) and is the only service publishing ports. nginx uses the Debian image (alpine has no `openssl`) and generates a self-signed pair into `./certs` when none exists | One-file install with HTTPS from the start; own-proxy setup remains documented |
