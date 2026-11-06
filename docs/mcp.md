# open-kase MCP endpoint

open-kase exposes its ticket workflow to AI agents and other Model Context Protocol (MCP) clients at
**`/mcp`** on the API server (e.g. `http://host:8080/mcp`). The endpoint uses the same credentials and the same
application services as the REST API (`/api/v1`): a tool can never read or change anything the same principal
could not read or change through REST. It contains no ticket logic of its own.

## Protocol and transport

| Item | Value |
| --- | --- |
| Transport | Streamable HTTP, `POST /mcp`, JSON responses (`application/json`) |
| SDK | `github.com/modelcontextprotocol/go-sdk` v1.8.0 (official Go SDK) |
| Protocol revisions | **2026-07-28** (pinned target) and, for older clients, 2025-11-25, 2025-06-18, 2025-03-26. 2024-11-05 (HTTP+SSE) is not offered |
| Sessions | **Stateless.** No `Mcp-Session-Id` is issued or required; every POST stands alone. `GET /mcp` and `DELETE /mcp` return `405` |
| Server → client | None: no sampling, elicitation, roots or standalone SSE stream. Tool results come back in the POST response |
| Limits | Request body ≤ 1 MiB; one tool call ≤ 30 s (then the tool returns `503 toolTimeout`); 120 requests per minute per principal with bursts of 30 (`429 rateLimited` + `Retry-After`) |

Because nothing is held between requests, any API process can answer any call and no sticky routing is needed.
The rate limit is counted in the API process and keeps state for at most 10,000 principals; restarting the
process resets it.

## Authentication and authorization

Send a bearer token on **every** request:

```
Authorization: Bearer <token>
```

| Credential | Who | Notes |
| --- | --- | --- |
| API token `kase_live_...` | External AI agents, tools and customer-facing systems | One kind of token; the admin picks the scopes under Settings, Integrations. A token holding a staff scope (`tickets:comment`, `tickets:internal`, `tickets:write` or `tickets:all`) acts as a bot under its own name; any other token is requester-side (see below) |
| User access token (JWT from `POST /api/v1/auth/login`) | Agents and administrators | Uses the user's role (admin or agent) and groups. Access tokens expire after the configured lifetime (default 10 minutes) and the MCP endpoint cannot refresh them, so this is practical for short sessions and testing; long-running clients should use an API token |

Every HTTP request is authenticated against MongoDB (revoked or unknown token → `401 invalidToken`; a disabled
user or revoked session → `401`), and every tool call re-resolves the role, group membership and scopes.
Revoking a token, disabling a user or removing a group membership therefore applies to the **next call**.

Any valid token may connect (there is no separate MCP scope). Every tool checks the **same scopes as the REST
route it mirrors**, through the same services, so a tool can never exceed the token's scopes and a staff token
never exceeds what a human agent may do (no claiming, redaction, reports or administration).
A missing scope answers `insufficientScope`, a user-only tool `userRequired`, exactly like REST.

| Tool | REST equivalent | Requester-side token | Staff token | Users |
| --- | --- | --- | --- | --- |
| `ticketsList`, `ticketsSearch`, `ticketsGet` | `GET /tickets`, `/search`, `/tickets/{t}` + `/messages` | `tickets:read`, public messages only | `tickets:read`; agent notes as well with `tickets:internal` | agents: their groups' tickets and open/awaiting-customer/on-hold tickets assigned to them; admins: all |
| `ticketsCreate` | `POST /tickets` | `tickets:create` (the requester writes the first message) | `tickets:create` (the bot writes it) | admins and agents |
| `ticketsFollowUp` | `POST /tickets/{t}/follow-up` | `tickets:create` + `tickets:read` | same | admins and agents |
| `ticketsCopy` | `POST /tickets/{t}/copy` | — (`staffOnly`) | `tickets:create` + `tickets:read` | admins and agents |
| `ticketsReply` | `POST /tickets/{t}/messages` (public) | `tickets:comment` (as the requester) | `tickets:comment` (as the bot; a staff reply) | admins and agents |
| `ticketsAddNote` | `POST /tickets/{t}/messages` (agent) | — (`insufficientScope`) | `tickets:internal` | admins and agents |
| `ticketsUpdate` | `PATCH /tickets/{t}` | `subject` only, on tickets it created | `tickets:write` (priority, tags, request type, `organisationId`) | admins and agents |
| `ticketsTransition` | `POST /tickets/{t}/transitions` | — (`userRequired`) | `tickets:write` | admins and agents |
| `ticketsAssign` | `POST /tickets/{t}/assignments` | — (`userRequired`) | `tickets:write`; the assignee must be an active member of the group, the bot never is | admins and agents |
| `ticketsClaim` | `POST /tickets/{t}/claim` | — | — (`userRequired`) | members of the ticket's group |
| `groupsList` | `GET /groups` | — (`userRequired`) | `tickets:read` (all groups) | agents: own groups; admins: all |
| `agentsList` | `GET /members` | — (`userRequired`) | `tickets:read` | admins and agents |
| `statusesList` | `GET /statuses` | any scope | any scope | admins and agents |
| `contactsList`, `contactsGet` | `GET /contacts`, `/contacts/{c}` | `contacts:read` | `contacts:read` | admins and agents |
| `contactsUpsert` | `POST /contacts` | `contacts:write` | `contacts:write` | admins and agents |
| `cannedRepliesList` | `GET /macros` | — (`userRequired`) | `tickets:read` | admins and agents |
| `tagsList` | `GET /tags` | `tags:read` or `tags:write` (counts 0) | `tickets:read`, `tags:read` or `tags:write` | admins and agents |
| `tagsCreate` | `POST /tags` | `tags:write` | `tags:write` | admins |
| `tagsDelete` | `DELETE /tags/{name}` | `tags:write` | `tags:write` | admins |
| `holdReasonsList` | `GET /hold-reasons` | `holdReasons:read` or `holdReasons:write` | `tickets:read`, `holdReasons:read` or `holdReasons:write` | admins and agents |
| `holdReasonsCreate` | `POST /hold-reasons` | `holdReasons:write` | `holdReasons:write` | admins |
| `holdReasonsUpdate` | `PATCH /hold-reasons/{h}` | `holdReasons:write` | `holdReasons:write` | admins |
| `holdReasonsDelete` | `DELETE /hold-reasons/{h}` | `holdReasons:write` | `holdReasons:write` | admins |
| `organisationsList`, `organisationsGet` | `GET /organisations`, `/organisations/{id}` | `organisations:read` or `organisations:write` | `tickets:read`, `organisations:read` or `organisations:write` | admins and agents |
| `organisationsCreate`, `organisationsUpdate`, `organisationsDelete` | `POST` / `PATCH` / `DELETE /organisations` | `organisations:write` | `organisations:write` | admins |

**Tag scopes:** `tags:read` and `tags:write` are separate from the ticket scopes (`tickets:all` does
not include them) and do not make a token staff. `tagsDelete` is annotated destructive: it removes the tag from
every ticket carrying it. Both writes are audited with the token and `tags:write`.

**Hold reason scopes:** `holdReasons:read` and `holdReasons:write` are separate from the ticket scopes (`tickets:all` does not include
them) and do not make a token staff. `holdReasonsDelete` is annotated destructive and fails with `holdReasonInUse` while a ticket is on hold
with the reason; every write is audited with the token and `holdReasons:write`. A ticket is put on hold with `ticketsTransition`
(`action: "hold"`, `holdReasonId` from `holdReasonsList`; `tickets:write`).

**Organisation scopes:** `organisations:read` and `organisations:write` are separate from the ticket scopes (`tickets:all` does not include
them) and do not make a token staff. `organisationsDelete` is annotated destructive and fails with `organisationInUse` while a contact or ticket
references the organisation (deactivate it with `organisationsUpdate` `active: false` instead); every write is audited with the token and `organisations:write`. Create the organisation first, then pass its ID as
`organisationId` to `contactsUpsert` and `ticketsCreate` (nothing is inferred).

There is one kind of token. "Staff token" in this page means a token holding a staff scope
(`tickets:comment`, `tickets:internal`, `tickets:write` or `tickets:all`); "requester-side token" means any other
token (see `docs/api-contract-v1.md`, "API tokens"). Requester replies need `tickets:replyAsRequester`.
`tickets:all` grants every `tickets:*` scope except `tickets:replyAsRequester`. Requester-side tokens never see agent
notes, the assignee or staff activity, and only ever post public messages. A ticket the caller cannot see answers
`ticketNotFound`, exactly like REST. There is one project per deployment, so no tool takes a project argument.

### Origin validation

Requests without an `Origin` header (CLI tools, server-side agents) are accepted. Browser requests are accepted
only when `Origin` equals the origin of the configured public base URL (`product.baseUrl` in settings), or when
both the `Origin` and the requested host are loopback (`localhost`, `127.0.0.1`, `[::1]`) for local development.
Anything else is rejected with `403 originRejected` before authentication (DNS-rebinding protection; matching the
request's `Host` is deliberately not enough).

## Tools

All tools return structured content (`structuredContent`, also serialized as text) using the REST shapes from
`docs/api-contract-v1.md` (`Ticket`, `Message`, `Group`, `Member`, `Status`, `Contact`, `Macro`). Errors are tool
results with `isError: true` whose text is the REST problem body, e.g.
`{"status":409,"code":"versionConflict","detail":"…","requestId":"…"}`. Unknown arguments are rejected. Tool
names are camelCase, `<resource><Verb>`.

Common arguments:

- `ticket` — ticket number (`12` or `#12`) or ID. Ticket outputs carry
  `number` (no `key`) and `organisation` `{id, name, externalRef}` (`null` when unset); `ticketsCreate` accepts an optional
  `organisationId` and `ccEmails` (set explicitly, never taken from the requester) and `ticketsUpdate` lets staff change or clear it (and `ccEmails`) (signed-in users get `403 organisationManagedByIntegration` on API-created tickets; tokens may).
- `expectedVersion` (ticket mutations) — the ticket `version` you based the change on; a newer ticket fails with
  `versionConflict`. When omitted, the tool reads the ticket first and uses its current version (a change
  between that read and the write still fails with `versionConflict`).
- `idempotencyKey` (ticket mutations) — 1–200 printable ASCII characters. Retrying with the same key and
  arguments returns the first result without repeating the change (kept 24 h, scoped to the principal and the
  tool); the same key with different arguments fails with `idempotencyKeyReused`.
- `cursor`, `limit` (lists) — opaque `nextCursor` of the previous page; 1–100 (default 25).

| Tool | Annotations | Arguments | Result |
| --- | --- | --- | --- |
| `ticketsList` | read-only | `view?` (`myWork`, `unassigned`, `watching`, `awaitingCustomer`, `onHold`, `dueSoon`, `resolved`, `archived`, `all`; user-only views are refused for tokens), `q?`, `groupId?`, `cursor?`, `limit?` | `{ items: Ticket[], nextCursor }` |
| `ticketsSearch` | read-only | `query` (search words, phrases in quotes, `-word` exclusions, or a ticket number; ≤ 100 chars), `cursor?`, `limit?` | `{ items: SearchHit[], nextCursor, total, totalCapped, exactMatch: Ticket \| null }` — the REST `/search` engine: subjects and the messages the caller may read, plain-text snippets with UTF-16 highlight ranges; `exactMatch` is the ticket named by a number (`42` or `#42`) |
| `ticketsGet` | read-only | `ticket`, `includeMessages?` (default true), `messagesCursor?`, `messagesLimit?` | `{ ticket: Ticket, messages?: { items: Message[], nextCursor } }` |
| `ticketsCreate` | not destructive | `subject`, `body`, `requester` `{ contactId }` or `{ name, email, externalId? }`, `priority?` (1–4), `requestTypeId?`, `groupId?`, `organisationId?`, `ccEmails?`, `idempotencyKey?` | `Ticket` (unassigned, in the group or Triage). From a requester-side token, `priority` is stored as `requestedPriority` |
| `ticketsCopy` | not destructive | `ticket`, `subject?`, `groupId?`, `priority?`, `requestTypeId?`, `tags?`, `body?`, `includeAttachments?`, `idempotencyKey?` | `Ticket` linked `copiedFrom` the original (also for archived tickets) |
| `ticketsFollowUp` | not destructive | `ticket`, `subject?`, `body?`, `priority?`, `groupId?`, `requestTypeId?`, `tags?`, `addSignature?`, `idempotencyKey?` | `Ticket` linked `followUpTo` the original (also for archived tickets) |
| `ticketsClaim` | not destructive | `ticket`, `expectedVersion?`, `idempotencyKey?` | `Ticket`; one concurrent claim wins, others get `ticketAlreadyClaimed` |
| `ticketsReply` | not destructive | `ticket`, **`audience: "public"`** (required), `body`, `expectedLastPublicId?`, `thenStatusId?`, `addSignature?` (users only), `as?` (`staff` \| `requester`; default staff with `tickets:comment`, otherwise requester), `contactId?` (with `as: requester` only: author the reply as another contact of the ticket's organisation; `contactNotInOrganisation` otherwise), `expectedVersion?`, `idempotencyKey?` | `{ message: Message, ticket: Ticket }`; `newerReplyExists` when another public message arrived after `expectedLastPublicId`; `thenStatusId` changes the status atomically with the message (users, staff tokens with `tickets:write`) |
| `ticketsAddNote` | not destructive | `ticket`, **`audience: "agent"`** (required), `body`, `thenStatusId?`, `expectedVersion?`, `idempotencyKey?` | `{ message: Message, ticket: Ticket }` |
| `ticketsUpdate` | not destructive, idempotent | `ticket`, `priority?` (1 critical … 4 low), `tags?` (replaces the set; added tags must be in `tagsList`), `requestTypeId?`, `subject?` (requester-side token, own tickets), `organisationId?`, `ccEmails?`, `expectedVersion?`, `idempotencyKey?` | `Ticket` |
| `ticketsTransition` | not destructive | `ticket`, `action` (`resolve`, `reopen`, `archive`, `hold`, `setStatus`), `statusId?` (only with `setStatus`; IDs from `statusesList`), `holdReasonId?` (required with `hold`; IDs from `holdReasonsList`; the ticket returns to Open after the reason's days), `note?` (with `hold`: an internal note), `reason?`, `expectedVersion?`, `idempotencyKey?` | `Ticket`; archived tickets are final (`ticketArchived`) |
| `ticketsAssign` | not destructive, idempotent | `ticket`, `groupId` (the current group to change only the assignee), `assigneeId?` or `unassign?`, `expectedVersion?`, `idempotencyKey?` | `Ticket`; without `assigneeId` an assignee who is a member of the target group is kept, otherwise cleared; an ineligible assignee is `validationFailed` (`assigneeId notEligible`) |
| `groupsList` | read-only | `cursor?`, `limit?` | `{ items: Group[], nextCursor }` |
| `agentsList` | read-only | `cursor?`, `limit?` | `{ items: Member[], nextCursor }` — each person's role, groups and active state |
| `statusesList` | read-only | — | `{ items: Status[] }` |
| `contactsList` | read-only | `q?` (name or email prefix), `cursor?`, `limit?` | `{ items: Contact[], nextCursor }` |
| `contactsGet` | read-only | `contactId` | `Contact` |
| `contactsUpsert` | not destructive, idempotent | `name`, `email`, `externalId?`, `organisationId?` (omitted: unchanged; empty: clears) | `{ contact: Contact, created: boolean }` (an existing contact with the email is updated) |
| `cannedRepliesList` | read-only | — | `{ items: Macro[] }` |
| `tagsList` | read-only | — | `{ items: Tag[] }` — the admin-managed catalog; only these tags can be added with `ticketsUpdate` (`unknownTag` otherwise) |
| `holdReasonsList` | read-only | — | `{ items: HoldReason[] }` — id, name, `maxDays` (the hold length), `inUse` (admins and `holdReasons:write` only) |
| `holdReasonsCreate` | not destructive | `name`, `maxDays` (1–365) | `HoldReason`; `holdReasonExists` when the name is taken |
| `holdReasonsUpdate` | not destructive | `holdReasonId`, `name?`, `maxDays?`, `expectedVersion?` | `HoldReason`; a new `maxDays` affects only later holds |
| `holdReasonsDelete` | destructive | `holdReasonId`, `expectedVersion?` | `{ deleted }`; `holdReasonInUse` while a ticket is on hold with it |
| `organisationsList` | read-only | `q?`, `active?` (`true` default, `false`, `all`), `cursor?`, `limit?` | `{ items: Organisation[], nextCursor }` — by name; counts for managers only |
| `organisationsGet` | read-only | `organisationId` | `Organisation` |
| `organisationsCreate` | not destructive | `name`, `externalRef?` | `Organisation`; `organisationExists` / `organisationRefTaken` |
| `organisationsUpdate` | not destructive | `organisationId`, `name?`, `externalRef?` (empty clears), `active?` (false deactivates), `expectedVersion?` | `Organisation` |
| `organisationsDelete` | destructive | `organisationId`, `expectedVersion?` | `{ deleted }`; `organisationInUse` while a contact or ticket references it |

Scheduled sending, watchers and links are not available through MCP (REST only, signed-in users).

The audience is never inferred: `ticketsAddNote` without `audience: "agent"` and `ticketsReply` without
`audience: "public"` fail; neither tool can post to the other audience. There are no ticket delete,
redaction or settings tools (tickets are never deleted; the delete tools only remove tags, hold reasons and organisations). Annotations are hints for clients; the server's authorization decides what actually
happens.

### Example calls

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ticketsCreate","arguments":{
  "subject":"Cannot receive external email","body":"Inbound mail bounces with 550 5.1.1.",
  "requester":{"name":"Priya Raman","email":"priya@acme.example"},"priority":2,"idempotencyKey":"crm-4711"}}}
```

```json
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ticketsAddNote","arguments":{
  "ticket":"12","audience":"agent","body":"MX records point to the old host."}}}
```

```json
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ticketsTransition","arguments":{
  "ticket":"12","action":"resolve","expectedVersion":7,"reason":"MX fixed"}}}
```

With curl (any revision from the table; this one shows the 2025-11-25 headers):

```sh
curl -s http://host:8080/mcp \
  -H "Authorization: Bearer $OC_TOKEN" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Connecting a client

1. An admin creates an API token under Settings → Integrations (or `POST /api/v1/integrations`). For an AI agent
   that works tickets, name it as requesters should see it (e.g. "Support bot") and pick
   the scopes it needs, e.g. `tickets:read`, `tickets:comment`, `tickets:internal` (or `tickets:all`). The token
   is shown once.
2. Point the client at `http(s)://host:8080/mcp` (or the proxied public URL) with a static header.

Claude Code:

```sh
claude mcp add --transport http open-kase http://host:8080/mcp \
  --header "Authorization: Bearer kase_live_..."
```

Other clients that accept a Streamable HTTP URL plus custom headers are configured the same way, for example:

```json
{ "mcpServers": { "open-kase": { "type": "http", "url": "http://host:8080/mcp",
  "headers": { "Authorization": "Bearer kase_live_..." } } } }
```

Behind a reverse proxy, forward `/mcp` like `/api/` (no WebSocket or long-lived streams are needed) and keep
request bodies ≤ 1 MiB. Use HTTPS for anything beyond a trusted LAN: the token is a bearer secret.

## Audit and logs

Every tool call writes an audit event `mcp.toolCalled` (actor, `target: mcpTool/<tool>`, project, ticket,
outcome, failure code as `reason`, and the **names** of the arguments sent) and one structured log line
(`msg="mcp tool call"` with request ID, tool, principal, project, ticket, outcome, code, duration). Message
bodies, contact details and other argument values are never written to audit events or logs. Mutations also
write their normal business audit events (`ticket.created`, `message.created`, …) exactly as over REST. For an
API token the actor is the token (type `service`, its ID and name) together with the **scopes that allowed the
call** (`actor.scopes`, e.g. `["tickets:internal"]`), also shown in the ticket's activity timeline.

## Known limits and compatibility

- **No OAuth.** The endpoint does not publish OAuth protected-resource metadata and has no authorization server.
  Clients must be configured with a static `Authorization: Bearer` header. Clients that can only connect to remote
  MCP servers through the MCP OAuth flow (for example "connector" UIs that do not allow custom headers) are **not
  supported**; on a `401` such clients may try OAuth discovery, which fails because no metadata is published.
- User access tokens expire quickly and cannot be refreshed through MCP; use API tokens for unattended clients. 
- Stateless mode means no server-initiated messages, no resumable streams and no list-changed notifications
  (the tool list only changes with a new release). Batch JSON-RPC requests are refused by revisions ≥ 2025-06-18.
- No attachments: tools cannot upload or download files; use the REST attachment routes.
- `ticketsSearch` uses the audience-scoped full-text engine of `GET /search` (no stemming) plus an
  exact ticket-number match; there is no knowledge-base search yet.
- The rate limit is per API process and in memory.
