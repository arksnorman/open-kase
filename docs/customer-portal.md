# Building a customer portal on open-kase

open-kase has no customer logins. If your own application (a SaaS dashboard, a member area) should let
customers open and follow tickets, it does so on their behalf through the REST API (or MCP) with one
integration token. This guide covers the setup that keeps each customer account's tickets separate.

## 1. Create the token

An admin creates it in the web app under Settings, Integrations (or `POST /api/v1/integrations`). Give it only these scopes:

| Scope | Why |
| --- | --- |
| `tickets:read` | list and read tickets and their public messages and attachments |
| `tickets:create` | open tickets for a contact (and attach files) |
| `tickets:replyAsRequester` | post customer replies, and resolve (close) a ticket |
| `contacts:read`, `contacts:write` | read, create and update the people on a customer account |
| `organisations:read`, `organisations:write` | read, create and update customer accounts |

**Do not grant `tickets:write`, `tickets:internal`, `tickets:comment` or `tickets:all`.** Any of those makes the token
a staff token: it then sees staff names and the assignee, tags and `version`, may set the effective priority, assign
and transfer tickets, archive, reopen and hold them, and (with `tickets:comment` or `tickets:all`) its messages are posted as a staff bot. `tickets:internal` also exposes agent notes. Without them the token is
**requester-side**: it sees public messages only, no assignee, tags or SLA, `version` is always `0`, a `priority` sent on create is only recorded as the
requested priority, and everything it writes is authored by a customer contact. Your portal shows customers what they are allowed to see; the token should not
be able to see more. Keep the token on your server, never in the browser.

## 2. One organisation per customer account, contacts with `externalId`

- Create one organisation per account in your application: `POST /organisations { name, externalRef }`, where
  `externalRef` is your account ID. Store the returned `id`.
- Create a contact for each person who uses the portal: `POST /contacts { name, email, externalId,
  organisationId }`. `externalId` is your user ID. `POST` upserts: it matches an existing contact by `externalId` first, then by email (the
  name, email and organisation link are then updated). If the `externalId` belongs to one contact and the email to
  another, or the email already carries a different `externalId`, it answers `409 contactConflict`. Store the returned `id`.
- Look up without writing: `GET /contacts?externalId=<your user ID>` and `GET /organisations?externalRef=<your account ID>`
  are exact matches (`acct-1` never returns `acct-10`). Deactivated organisations only appear with `active=all`.
- When an account closes, deactivate the organisation (`PATCH /organisations/{o} { active: false }`). Organisations
  in use cannot be deleted. `PATCH /organisations/{o}` needs `If-Match: <version>` (the `version` from the organisation).

## 3. Set `organisationId` on every ticket

```http
POST /api/v1/tickets
Authorization: Bearer kase_live_...
Idempotency-Key: 9b1c...

{ "subject": "Invoice shows the wrong VAT number",
  "body": "Our March invoice ...",
  "requester": { "contactId": "<contact id>" },
  "organisationId": "<organisation id>",
  "ccEmails": ["accounts@customer.example"] }
```

open-kase **never copies the organisation from the contact**: a ticket without `organisationId` has no
organisation, does not show up in the account's list and cannot take replies from teammates (below). Always send
it. Agents cannot change the organisation of a ticket your integration created.

## 4. List, reply, close

- **List an account's tickets:** `GET /tickets?organisationId=<id>` (combine with `view=resolved`,
  `view=archived`, `statusId=` and `cursor=` as needed). `GET /tickets?requesterId=<contact id>` lists one person's tickets. These filters work with
  the portal token; assignee, tag, priority, request type and due filters do not (`403 userRequired`). Before showing a ticket opened by ID, check that its
  `organisation.id` is the signed-in customer's organisation: the token can read every ticket, so your portal is
  the access check.
- **Reply as a customer:** `POST /tickets/{t}/messages { "body": "...", "visibility": "public", "as": "requester" }`.
  Add `"contactId": "<id>"` when the person replying is not the ticket's requester; the contact must belong to the
  ticket's organisation (otherwise `422 contactNotInOrganisation`, also for an unknown contact) and the message is authored under their name. A
  customer reply reopens a resolved ticket, ends a hold and never counts as a first response. Like a requester reply, a
  teammate reply emails nobody on the customer side: notify the others yourself from the `ticket.message.public` webhook
  (section 5: `message.author.id`, `requester` and `ccEmails`).
- **Attachments:** the token can upload with `POST /attachments` (multipart, field `file`), pass the returned IDs as `attachmentIds` when
  creating a ticket or posting a message, and download them with `GET /attachments/{a}/download`.
- **Close:** resolve with `POST /tickets/{t}/transitions { "action": "resolve" }`. The portal token needs no `If-Match` (it sees no version). Resolving a
  ticket also ends a hold. Resolving a ticket that is already resolved is `409 invalidTransition`; the customer reopens it by replying. Archiving happens automatically through auto-archive (`archiveAfterResolvedDays`, set by an admin) or by agents; archived is final, so to continue the conversation create a
  follow-up (`POST /tickets/{t}/follow-up`).

## 5. Send notifications yourself (optional)

By default open-kase emails requesters and CC addresses (ticket received, new reply, status changes, follow-up
reminders). If your application sends its own emails or in-app notifications:

1. Turn off **Email contacts and CC addresses** under Settings, Configuration, General (`notifications.requesterEmails: false` in
   `PUT /settings`; an absent `requesterLinkBaseUrl` keeps the stored link). Contacts and CC addresses then get no email from open-kase; each suppressed email is listed as
   a skipped delivery with reason `requesterEmailsOff`. Staff emails are unchanged. While SMTP is not enabled, deliveries are
   recorded as `smtpNotConfigured` instead, whatever the switch says.
2. Register a webhook endpoint (Settings, Webhooks) for `ticket.created`, `ticket.message.public`,
   `ticket.statusChanged` and `ticket.followupReminder`. Each of these payloads carries `requester`
   (`id`, `name`, `email`, `externalId`), `organisation` (`id`, `name`, `externalRef`, or `null`), `subject` and
   `ccEmails`, so you can notify the right people without calling the API back. In `ticket.message.public`, `message.author` is
   `{context, id, name}`; `id` is the contact's ID for a customer author (compare it with `requester.id` to see a teammate), so notify the
   requester, the CC addresses and the other contacts except the author.
3. `ticket.statusChanged` carries `by`: `"requester"` when the customer resolved the ticket with the portal token (or a
   reply reopened it), `"staff"` for agents and staff tokens, `"automation"` for auto-resolve, auto-archive and a
   hold that ended. Skip notifying the customer when `by` is `"requester"`: they just did it themselves.
4. Handle `ticket.followupReminder`: open-kase is waiting for the customer. When `autoResolveNotice` is `true` this
   is the last reminder and the ticket will be resolved automatically if nobody replies; tell the customer so.

Delivery is at least once: de-duplicate on `eventId` and use `ticket.revision` to ignore events with a *lower* revision (follow-up reminders do not advance it). See
[webhooks.md](webhooks.md) for authentication and retries, and [api-contract-v1.md](api-contract-v1.md) for the
full API.
