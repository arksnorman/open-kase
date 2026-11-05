# Receiving open-kase webhooks

This document is for developers of systems that receive outbound webhooks from an
open-kase installation. An administrator registers an endpoint URL, selects event types and,
optionally, configures extra headers, authentication (None, Basic or Token) and TLS settings
(a custom certificate authority and/or a client certificate). open-kase then sends an HTTPS
`POST` for each selected event.

Requests are **not signed**. Authenticity comes from HTTPS plus the authentication you ask the administrator to
configure — see [Authenticating deliveries](#authenticating-deliveries).

## Request

```
POST /your/endpoint HTTP/1.1
Content-Type: application/json
User-Agent: open-kase-webhooks/<version>
X-Open-Kase-Event-Id: 6703f1c2a4b5c6d7e8f90a1b
X-Open-Kase-Event-Type: ticket.statusChanged
Authorization: Bearer <token>        (only with Token or Basic authentication)
X-Api-Key: <value>                   (any extra headers configured for the endpoint)
```

| Header | Meaning |
| --- | --- |
| `X-Open-Kase-Event-Id` | Stable event identifier. Identical on every retry of the same event; use it to de-duplicate. |
| `X-Open-Kase-Event-Type` | The event type (same as `type` in the body). |
| `Authorization` | `Basic base64(username:password)` with Basic authentication, `Bearer <token>` with Token authentication; absent with None (unless configured as an extra header). |
| Extra headers | Up to 20 name/value pairs configured by the administrator, sent unchanged on every request. |

Extra headers cannot override `Host`, `Content-Length`, `Content-Type`, `Transfer-Encoding`,
`Connection` and other hop-by-hop headers, the `X-Open-Kase-Event-*` headers, or
`Authorization` while Basic or Token authentication is selected. A configured `User-Agent`
replaces the default one.

## Payload envelope

```json
{
  "schemaVersion": 1,
  "eventId": "6703f1c2a4b5c6d7e8f90a1b",
  "type": "ticket.statusChanged",
  "occurredAt": "2026-10-02T08:00:00.123Z",
  "ticket": { "id": "6703e9aa0b1c2d3e4f506172", "number": 42, "revision": 7 },
  "data": { "status": "resolved" }
}
```

| Field | Meaning |
| --- | --- |
| `schemaVersion` | Envelope version. Currently `1`. An incompatible change gets a new version. Ignore fields you do not recognise. |
| `eventId` | Same value as the `X-Open-Kase-Event-Id` header. |
| `type` | Event type (see "Event types" below). You receive only the types selected for your endpoint, plus `webhook.test` when an administrator sends a test delivery. |
| `occurredAt` | When the change was committed (RFC 3339, UTC, millisecond precision). |
| `ticket` | Ticket identifiers when the event concerns a ticket: `id` and `number` (shown to people as `#42`). `revision` is the ticket version after the change. There is no ticket `key` and no `project` object (one project per deployment). |
| `data` | Minimized, event-specific fields that the endpoint is authorized to receive. |

Payload content rules:

- Payloads contain only data the endpoint's integration principal is allowed to read. Every
  endpoint is backed by its own read-only service principal (`tickets:read`, public view);
  public content is the default.
- Agent-only notes are never included unless an administrator enabled
  `includeAgentNotes` on the endpoint. Only then can `ticket.message.agentNote` be selected,
  and only then do its payloads carry the note body. Without that grant, agent-note events are
  not sent at all, and no other event carries agent-note content.
- Secrets, tokens, credentials and password material never appear in payloads.
- Treat `data` as a notification, not a full record. Fetch the current state through the
  REST API (`/api/v1`) with your integration credential when you need more fields.

## Event types

`data` is built per endpoint from what
its principal may read at the first delivery attempt and then stored, so retries send identical
bytes. Values describing the change (status, group, subject, priority) are taken from the state
recorded with the event, not from a later state.

| Type | Sent when | `data` |
| --- | --- | --- |
| `ticket.created` | A ticket was created (by an agent or through the API) | `subject`, `priority` (1–4), `requestedPriority` (when an integration asked for one), `status` `{id, name, category}`, `group` `{id, name}`, `requestType` `{id, name}`, `source` (`web`/`api`), `requester` `{id, name, email, externalId}`, `organisation` `{id, name, externalRef}` (`null` when none), `createdAt` |
| `ticket.updated` | Subject, priority, request type or organisation changed | `changes` (field names), `subject`, `priority`, `requestType` `{id}`, `organisation` `{id, name, externalRef}` (`null` when none) |
| `ticket.message.public` | A public reply was added by staff, an agent token (bot) or the requester | `message` `{id, visibility: "public", author {context: staff\|bot\|contact, name}, body (raw Markdown), attachments [{id, filename, size, contentType}], createdAt}`. A file a redaction removed before the payload was built (`removeAttachments`) is `{id, removed: true}` with no other member |
| `ticket.message.agentNote` | An agent-only note was added — **only with `includeAgentNotes`** | same shape as above with `visibility: "agent"` and the note body |
| `ticket.assigned` | The ticket was claimed, released, assigned or moved to another group | `group` `{id, name}`; with `includeAgentNotes` also `assignee` `{id, name}` or `null`. Staff identities are not sent to public endpoints |
| `ticket.statusChanged` | Resolve, reopen, archive, set status, or a requester reply that reopened the ticket | `status` `{id, name, category}`, `transition` (`resolve`, `reopen`, `archive`, `hold`, `holdEnded`, `setStatus`, `requesterReply`) |
| `ticket.slaWarning` | The ticket's first response is due soon (the SLA's warning share of the target has passed) | `sla` `{kind: firstResponse, state: "dueSoon", dueAt}`, `priority` |
| `ticket.slaBreached` | The first-response deadline was missed (sent once per deadline) | `sla` `{kind, state: "breached", dueAt}`, `priority` |
| `webhook.test` | An administrator used "test delivery" | `test: true`, `message` (no `ticket`) |

`GET /api/v1/webhook-event-types` lists the selectable types with descriptions.

Note on `ticket.revision`: it is the ticket's internal version after the event. Every change,
including staff-only activity, advances it, so a gap between revisions only tells you that
something you were not sent happened in between — never what it was.

## Authenticating deliveries

open-kase does not sign request bodies. Verify that a request comes from your open-kase
installation with one or more of these, configured per endpoint on the admin Webhooks page or
through `POST/PATCH /api/v1/webhooks`:

| Option | What open-kase sends | What your receiver checks |
| --- | --- | --- |
| **Token** (`auth.type: "token"`) | `Authorization: Bearer <token>` | Constant-time comparison with the token you issued. |
| **Basic** (`auth.type: "basic"`) | `Authorization: Basic base64(username:password)` | The username and password (most web servers and gateways support this natively). |
| **Extra header** | e.g. `X-Api-Key: <value>` | The header value (useful for API gateways that expect a specific header). |
| **Client certificate** (mutual TLS) | A client certificate during the TLS handshake | Require and verify client certificates issued by your CA (strongest option; nothing secret travels in headers). |

Always use HTTPS so these credentials are encrypted in transit. Plain HTTP is accepted only by
installations running in development mode.

Credentials are write-only: the password, token, header values and client private key are stored
encrypted with the installation's master key and are never returned by the API or shown again in
the UI (responses report `passwordConfigured`, `tokenConfigured`, `valueConfigured`,
`clientKeyConfigured`). On update, a header sent without a value, or a password/token/client key
left empty, keeps the stored value. To rotate a credential, accept both the old and the new value
in your receiver, update the endpoint, then remove the old value.

Stored secrets are only ever sent to the destination they were entered for. If an update changes the
URL's scheme, host or port, or replaces the custom CA with a different one, the Basic password or
Token and the value of every extra header must be entered again in the same request; otherwise the
API answers `422` (`required` on `auth` or `headers[i].value`, "Enter the credentials again: the
destination changed."). Switching authentication to None needs no re-entry, nor does a path-only
change on the same host. The client key is not sent to the receiver and is not affected.

### TLS options

- **Custom certificate authority** (`tls.caCertificate`): one or more PEM certificates trusted
  **in addition to** the system root store, for receivers whose certificate comes from a private
  CA. Public certificates keep working. The certificate is still verified for the URL's hostname;
  verification can never be disabled.
- **Client certificate** (`tls.clientCertificate` + `tls.clientKey`, PEM): presented when your
  server asks for a client certificate. open-kase checks on save that both parse and that the
  key matches the certificate (otherwise `422` with a field error). Removing the certificate also
  removes its key.
- The API and the admin page show each certificate's subject, issuer and expiry so you can renew in
  time. An expired or untrusted certificate makes deliveries fail with error class `tls`.

## Deduplication and ordering

- **Deduplicate by event ID.** Delivery is *at least once*: a network failure after your
  server processed a request, or a worker restart, can cause the same event to be sent
  again. Store processed `eventId` values (with a unique constraint) for at least as long
  as the retry window (about 15 hours; keeping them for 7 days is a safe default) and treat
  a repeated ID as already handled: return `2xx` without processing it again.
- **No ordering guarantee.** Events can arrive out of order, especially after retries. Use
  `ticket.revision`: ignore an event whose revision is lower than the revision you already
  stored for that ticket, or fetch the current ticket from the REST API.

## Responses, retries and limits

| Your response | open-kase behavior |
| --- | --- |
| `2xx` within 10 seconds | Delivered. |
| `410 Gone` | Not retried; the delivery is dead-lettered. |
| `429`, `5xx`, other `4xx`, timeout, connection or TLS error | Retried. |
| `3xx` | Redirects are **not** followed; treated as a failure and retried. Register the final URL. |

- Retries follow 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours after successive
  failures, each with ±20 % jitter. After the sixth failed attempt the delivery moves to a
  dead-letter state that an administrator can retry manually from the admin Webhooks page.
- A `Retry-After` header (seconds or HTTP date) on a retryable response can lengthen the next
  delay, capped by the installation (12 hours by default). It never shortens the schedule.
- Retries keep the same `eventId` and the same body bytes. Each attempt uses the endpoint's
  current headers, authentication and TLS settings; destination permissions are rechecked before
  each retry.
- The whole request, including reading your response, must finish within 10 seconds. Respond
  quickly and process asynchronously.
- At most 64 KiB of your response body is read; the first 256 bytes may be shown to
  administrators in delivery history. Do not return sensitive data in webhook responses.

## Endpoint requirements

- HTTPS with a certificate valid for the hostname, issued by a public CA or by a CA configured
  for the endpoint. Certificate verification cannot be disabled. Plain HTTP is accepted only by installations running in development mode.
- No credentials in the URL (`https://user:pass@…` is rejected); use Basic or Token
  authentication, an extra header or a client certificate instead.
- The destination must resolve to a public or private-network address (RFC 1918,
  unique-local IPv6, CGNAT `100.64.0.0/10`); only administrators configure webhooks, and
  receivers on the LAN need no extra setting. open-kase checks every resolved IP address
  at connection time and refuses loopback (allowed only in development mode), link-local,
  multicast, unspecified, documentation and other reserved ranges. Cloud metadata addresses
  such as `169.254.169.254` are always refused.
- Outgoing webhook requests do not use HTTP proxies from the environment.
