# Building a customer portal on open-kase

open-kase has no customer logins. If your own application (a SaaS dashboard, a member area) should let
customers open and follow tickets, it does so on their behalf through the REST API (or MCP) with one
integration token. This guide covers the setup that keeps each customer account's tickets separate.

## 1. Create the token

An admin creates it in the web app under Settings, Integrations (or `POST /api/v1/integrations`). Give it only these scopes:

| Scope | Why |
| --- | --- |
| `tickets:read` | list and read tickets and their public messages |
| `tickets:create` | open tickets for a contact |
| `tickets:replyAsRequester` | post customer replies |
| `tickets:write` | resolve and archive tickets, and set the ticket organisation |
| `contacts:read`, `contacts:write` | create and update the people on a customer account |
| `organisations:read`, `organisations:write` | create and update customer accounts |

**Never grant `tickets:internal` or `tickets:comment`.** `tickets:internal` exposes agent notes, and
`tickets:comment` posts as staff. Your portal shows customers what they are allowed to see; the token should not
be able to see more. Keep the token on your server, never in the browser.

`tickets:write` makes this a staff token: responses include `version` (needed for `If-Match`), assignee and tags.
Without `tickets:internal` it still never sees agent notes.

## 2. One organisation per customer account, contacts with `externalId`

- Create one organisation per account in your application: `POST /organisations { name, externalRef }`, where
  `externalRef` is your account ID. Store the returned `id`.
- Create a contact for each person who uses the portal: `POST /contacts { name, email, externalId,
  organisationId }`. `externalId` is your user ID; `POST` upserts by email. Store the returned `id`.
- When an account closes, deactivate the organisation (`PATCH /organisations/{o} { active: false }`). Organisations
  in use cannot be deleted.

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
  `view=archived`, `statusId=` and `cursor=` as needed). Before showing a ticket opened by ID, check that its
  `organisation.id` is the signed-in customer's organisation: the token can read every ticket, so your portal is
  the access check.
- **Reply as a customer:** `POST /tickets/{t}/messages { "body": "...", "visibility": "public", "as": "requester" }`.
  Add `"contactId": "<id>"` when the person replying is not the ticket's requester; the contact must belong to the
  ticket's organisation (otherwise `422 contactNotInOrganisation`) and the message is authored under their name. A
  customer reply reopens a resolved ticket, ends a hold and never counts as a first response.
- **Close:** resolve, then archive. `POST /tickets/{t}/transitions { "action": "resolve" }` with
  `If-Match: <version>`, then later `{ "action": "archive" }` with the new version. Archived tickets are final; to
  continue the conversation create a follow-up (`POST /tickets/{t}/follow-up`).

## 5. Send notifications yourself (optional)

By default open-kase emails requesters and CC addresses (ticket received, new reply, status changes, follow-up
reminders). If your application sends its own emails or in-app notifications:

1. Turn off **Requester emails** under Settings, Configuration (General section) (`notifications.requesterEmails: false` in
   `PUT /settings`). Contacts and CC addresses then get no email from open-kase; each suppressed email is listed as
   a skipped delivery with reason `requesterEmailsOff`. Staff emails are unchanged.
2. Register a webhook endpoint (Settings, Webhooks) for `ticket.created`, `ticket.message.public`,
   `ticket.statusChanged` and `ticket.followupReminder`. Each of these payloads carries `requester`
   (`id`, `name`, `email`, `externalId`), `organisation` (`id`, `name`, `externalRef`, or `null`), `subject` and
   `ccEmails`, so you can notify the right people without calling the API back.
3. Handle `ticket.followupReminder`: open-kase is waiting for the customer. When `autoResolveNotice` is `true` this
   is the last reminder and the ticket will be resolved automatically if nobody replies; tell the customer so.

Delivery is at least once: de-duplicate on `eventId` and use `ticket.revision` to ignore older events. See
[webhooks.md](webhooks.md) for authentication and retries, and [api-contract-v1.md](api-contract-v1.md) for the
full API.
