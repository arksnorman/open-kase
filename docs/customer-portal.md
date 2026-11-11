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
  customer reply reopens a resolved ticket, ends a hold and never counts as a first response. While **Email contacts and CC addresses** is on, open-kase emails the rest of the customer
  side about the reply: the CC addresses when the requester wrote it, the requester and the CC addresses (except the
  teammate's own address) when a teammate wrote it. The email never contains the message text. With the switch off, notify the others yourself (section 5).
- **Attachments:** the token can upload with `POST /attachments` (multipart, field `file`), pass the returned IDs as `attachmentIds` when
  creating a ticket or posting a message, and download them with `GET /attachments/{a}/download`.
- **Close:** resolve with `POST /tickets/{t}/transitions { "action": "resolve" }`. The portal token needs no `If-Match` (it sees no version). Resolving a
  ticket also ends a hold. While the email switch is on, open-kase emails the rest of the customer side when the customer side resolves: the CC addresses when the requester closed it, the requester and the CC addresses (except the teammate's own address) when a teammate did. The assignee and watchers are told as before. With the switch off, tell them yourself (section 5). Resolving a ticket that is already resolved answers `200` with the ticket and changes nothing (an archived ticket is `409 ticketArchived`); the customer reopens it by replying.
  Add `"contactId": "<id>"` when the person closing it is not the ticket's requester; the same rule as for replies applies (the contact must belong to the ticket's organisation, otherwise `422 contactNotInOrganisation`; any other caller gets `422 notAllowed`). The `ticket.resolved` audit event records the contact, and the `ticket.statusChanged` webhook carries it as `contactId`.
  Archiving happens automatically through auto-archive (`archiveAfterResolvedDays`, set by an admin) or by agents; archived is final, so to continue the conversation create a
  follow-up (`POST /tickets/{t}/follow-up`). The follow-up copies the requester. With a portal token (one without `tickets:comment`) its first message is written as the requester; add `"contactId": "<id>"` to open it as another contact of the original's organisation (`422 contactNotInOrganisation` otherwise; users and tokens that write as the bot get `422 notAllowed`). That contact then is the requester of the new ticket and writes its first message. The organisation stays the original's. The copied CC list drops the contact's own address; the original requester is not added to it. The "received" email goes to the new requester and the CC addresses, and the `ticket.created` webhook shows the teammate as `requester`.

## 5. Send notifications yourself (optional)

By default open-kase emails requesters and CC addresses (ticket received, new reply, status changes, follow-up
reminders). It also emails the customer side about what the customer side did: a public reply or a resolve
goes to the requester and the CC addresses, never to the person who acted. You do not need to send those notices.
If your application prefers to send its own emails or in-app notifications, do this and turn the switch off:

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
   customer reply moved it back to Open), `"staff"` for agents and staff tokens, `"automation"` for auto-resolve,
   auto-archive and a hold that ended. With the switch off, open-kase emails nobody on the
   customer side, so for `by: "requester"` the notices are yours to send:
   - `transition: "resolve"`: tell the CC addresses and the other contacts, and the requester too when `contactId`
     is present (the teammate who resolved it; the key is absent when the requester did). Skip the person who acted.
   - `transition: "requesterReply"`: send nothing more. The reply itself arrives as `ticket.message.public` and the
     notice from step 2 covers it (`contactId` equals that message's `author.id` when a teammate replied).

   `ticket.created` never carries `contactId`: a follow-up opened by a teammate has the teammate as `requester`.
4. Handle `ticket.followupReminder`: open-kase is waiting for the customer. `reminder` is the number of this reminder in
   the current waiting period (1 for the first). When `autoResolveNotice` is `true` this
   is the last reminder and the ticket will be resolved automatically if nobody replies; tell the customer so.

Delivery is at least once: de-duplicate on `eventId` and use `ticket.revision` to ignore events with a *lower* revision (follow-up reminders do not advance it). See
[webhooks.md](webhooks.md) for authentication and retries, and [api-contract-v1.md](api-contract-v1.md) for the
full API.
