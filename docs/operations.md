# Operations

Day-to-day operation of one open-kase installation: MongoDB (single-node replica set), Redis and
the `open-kase` process. Installation is in [`install.md`](install.md). This is a single-host
deployment: when the host, MongoDB or the application is down, the helpdesk is down.

## Start, stop, status

| | systemd (native) | Docker Compose (service `open-kase`) |
| --- | --- | --- |
| Start | `systemctl start open-kase` | `docker compose up -d` |
| Stop | `systemctl stop open-kase` | `docker compose stop open-kase` |
| Restart | `systemctl restart open-kase` | `docker compose restart open-kase` |
| Status | `systemctl status open-kase`, `open-kase doctor` | `docker compose ps`, `docker compose run --rm open-kase doctor` |
| Logs | `journalctl -u open-kase -f` | `docker compose logs -f open-kase` |

`SIGTERM` drains: the server stops accepting connections, finishes in-flight requests for up to
20 s, closes live-update streams with a `reconnect` event, and stops the workers after their
current job (an interrupted job's lease expires and another attempt runs later). Exit code 0 on a
clean stop.

## Health checks

Unauthenticated, no details beyond check names:

- `GET /health/live`: the process is up.
- `GET /health/ready`: 200 only with a reachable replica-set primary, an initialized installation
  whose master key matches, and a valid settings snapshot. Redis is reported as `degraded` without
  failing readiness.

`open-kase health` probes `http://127.0.0.1:8080/health/ready` by default and exits 0 on 200. If
`OPEN_KASE_HTTP_ADDR` uses another port (or a non-loopback-reachable address), pass
`--url http://127.0.0.1:<port>/health/ready`; the Docker image uses it as its `HEALTHCHECK`. `open-kase doctor` checks the same plus
configuration warnings (missing base URL, SMTP not configured, single-node replica set).

## Backup and restore

All state is in one MongoDB database (`open_kase`: tickets, messages, settings, users, audit, jobs
and attachment bytes in GridFS) plus one secret outside it, the **master key**
(`OPEN_KASE_MASTER_KEY_FILE`). Redis holds nothing that needs a backup.

**Back up the master key separately from the database dump.** The dump contains secrets encrypted
with the key (SMTP password, webhook credentials, MFA secrets, JWT signing keys). Without the key
they cannot be decrypted and the application refuses to become ready after a restore. With both
files, anyone can read those secrets, so never store the key next to the archive: keep it in a
password manager or another location with different access. The key does not change unless you
replace it, so one safe copy covers all archives.

### Making a backup

Docker Compose, from the directory with `compose.yaml` (MongoDB has no published port, so run the
tools inside its container, authenticating as the root user):

```sh
docker compose exec -T mongo sh -c 'mongodump -u root -p "$(cat /run/secrets/mongo_root_password)" --authenticationDatabase admin --db open_kase --archive --gzip' > open-kase-$(date -u +%Y%m%dT%H%M%SZ).archive.gz
# secrets/master.key is owned by uid 65532 (the container user) with mode 0600, so root must read it:
sudo cp secrets/master.key /safe/place/open-kase-master.key   # once, and after any change
```

Native, with `mongodump` from the MongoDB Database Tools (add credentials to the URI if your
mongod requires them):

```sh
mongodump --uri 'mongodb://127.0.0.1:27017/open_kase?replicaSet=rs0' --archive --gzip > open-kase-$(date -u +%Y%m%dT%H%M%SZ).archive.gz
sudo cp /etc/open-kase/master.key /safe/place/open-kase-master.key
```

Check that the archive is readable: `gzip -t file.archive.gz`.

**Consistency.** The database is dumped collection by collection while the application keeps
running, so a ticket created during the dump can appear without its newest messages. Restored data
stays valid for the application, but the snapshot is not from one instant. For a consistent
snapshot either stop the application for the duration of the dump (seconds for a small
installation: `docker compose stop open-kase`, then `start`), or add `--oplog` to `mongodump` and
`--oplogReplay` to `mongorestore`, which dumps the whole deployment (all databases) instead of one
database; `--oplog` cannot be combined with `--db`, so drop `--db open_kase` in that case.

Schedule the dump (cron or a systemd timer), copy archives off the host, and test a restore
regularly. Retention is up to you.

### Restoring

1. Stop the application: `docker compose stop open-kase` (or `systemctl stop open-kase`).
2. Put the original master key back where the application reads it (`secrets/master.key` for
   Compose, `/etc/open-kase/master.key` for systemd), with the same ownership and mode as before.
3. Restore. `--drop` replaces collections that already exist:

   ```sh
   # Docker Compose
   docker compose exec -T mongo sh -c 'mongorestore -u root -p "$(cat /run/secrets/mongo_root_password)" --authenticationDatabase admin --archive --gzip --drop' < open-kase-20261102T020000Z.archive.gz
   # Native
   mongorestore --uri 'mongodb://127.0.0.1:27017/?replicaSet=rs0' --archive --gzip --drop < open-kase-20261102T020000Z.archive.gz
   ```

4. Start the application and check `/health/ready` and `open-kase doctor`. `masterKey: fail` in
   the readiness output means the key does not match the restored database.

To rehearse a restore without touching production, restore into another database name with
`--nsFrom 'open_kase.*' --nsTo 'open_kase_copy.*'` and point a separate instance at it. Never run
two instances against the same database.

### What is not in a backup

- Redis (cache and presence hints; rebuilt automatically).
- Emails and webhook deliveries that were already sent. After restoring an older backup,
  deliveries that were pending at backup time are retried, so recipients may get some
  notifications twice (delivery is at least once).
- Sessions created after the backup: those users sign in again.
- Configuration outside MongoDB: `compose.yaml`, the environment file, the systemd unit, the
  reverse proxy.

## Background work

`serve` runs the outbox dispatcher and job workers (email, webhooks, SLA timers, lifecycle
automation) in the same process, plus a 15-minute maintenance loop for expired staged uploads and
automation scans. Inspect jobs with `open-kase jobs list [--state dead]`; requeue a dead-lettered
job with `open-kase jobs retry <id>`. Email and webhook deliveries are also visible to
administrators under `GET /api/v1/deliveries` and per webhook endpoint. With `serve --workers=false`
run `open-kase worker` separately.

## Upgrades

See [`install.md`](install.md#upgrading). In short: back up, pull the new image (or replace the
binary), restart; `EnsureSchema` creates missing collections and indexes at start. There are no
migrations. Rollback is restoring the backup taken before the upgrade and starting the older
version; data written after the backup is lost.

## Keys and secrets

**Master key** (`OPEN_KASE_MASTER_KEY_FILE`, 32 random bytes, base64). Encrypts the SMTP password,
webhook credentials, JWT signing keys and MFA secrets in MongoDB. Created by `init` (or by you
with `openssl rand -base64 32`), mode 0600. **Rotating the master key is not supported**: replacing
it makes every sealed value unreadable and the application not ready. If the key may be
compromised, restrict access to MongoDB and its backups and rotate the secrets it protects at
their source (SMTP password, webhook credentials at the receiver and then on the endpoint,
`open-kase keys rotate-jwt`, ask users to re-enrol MFA); the stored ciphertexts are then useless
even with the old key.

**JWT signing key.** `open-kase keys rotate-jwt` creates a new active key; the previous one keeps
verifying already issued access tokens for their lifetime (10 minutes by default) plus the
key-cache interval, so nobody is signed out. Run it periodically or after a suspected leak (then
also revoke sessions by changing the affected users' passwords or disabling and re-enabling them).

**Webhook credentials.** Requests are not signed. To rotate a token, password, header value or
client certificate, make the receiver accept both old and new values, update the endpoint
(`PATCH /api/v1/webhooks/{w}` or the Webhooks page), then retire the old value.

**API tokens.** Delete and recreate under Settings → Integrations; a token is shown once.

## Requester emails

Settings → Configuration → Email has a **Email contacts and CC addresses** switch
(`notifications.requesterEmails`, default on). When it is off, no email goes to requester contacts or
CC addresses (ticket received, replies, resolved/archived notices, follow-up reminders); staff emails
are unchanged. Each suppressed email is recorded as a skipped delivery with error class
`requesterEmailsOff`, checked at send time, so jobs queued before the switch are skipped too. Use
webhooks (including `ticket.followupReminder`, see `webhooks.md`) to notify requesters instead.

## SMTP

Set it under Settings → Configuration → Email (and set the base URL under General; email links
need it). Until SMTP is configured, deliveries are recorded as skipped.

- TLS mode is `starttls` (default, port 587), `tls` (implicit TLS, usually port 465) or `none`.
  Authenticated SMTP requires TLS.
- A relay with a private CA: paste its certificate (PEM, one or more) into **CA certificate**. It
  is trusted in addition to the system roots.
- A relay with a self-signed certificate you cannot export: **skip certificate verification**
  disables the server certificate check. Prefer the CA certificate; skipping verification allows
  anyone on the network path to impersonate the relay. Neither setting applies with TLS mode
  `none`.
- The stored password is never sent to a new destination: changing the server, port, user name or
  TLS mode, or weakening verification, requires entering the password again.
- Use the test button on the page after saving. Failed deliveries are retried with backoff
  (`retrying`, with the error class), then marked `failed` and can be retried by hand.

## Logs

One structured logger: JSON in production (`APP_ENV=production`), text otherwise;
`OPEN_KASE_LOG_LEVEL` (`debug`, `info`, `warn`, `error`) and `OPEN_KASE_LOG_FORMAT` override. Each
request produces one `http request` record (request ID, method, route, path without query string,
status, duration, bytes, client IP). Clients may send `X-Request-ID` (8 to 64 safe characters) and
receive it back, and problem responses carry `requestId`. Workers log job outcomes with error
classes.

Logs never contain passwords, tokens, cookies, API keys, secrets, message bodies or search terms.
Audit events (who changed what, by field name) are stored separately in MongoDB (`auditEvents`),
not in the logs. In Docker, logs go to the container's stdout and are read with
`docker compose logs`; configure a log driver or rotation in Docker if you need retention.

## Rate limits and throttling

Per-principal limits (users and integrations) on expensive routes and per-client-IP limits on the
unauthenticated `/auth/*` routes are answered with `429 rateLimited` and `Retry-After`:

| Class | Routes | Sustained | Burst |
| --- | --- | --- | --- |
| `auth` (per IP) | login, refresh, logout, MFA, password reset, invitation accept | 60/min | 30 |
| `ticketCreate` | create ticket, follow-up | 120/min | 60 |
| `messagePost` | post message, scheduled message, send now | 120/min | 60 |
| `upload` | stage attachment | 60/min | 30 |
| `search` | full-text search | 120/min | 60 |
| `connectionTest` | SMTP/webhook tests, SLA backfill | 6/min | 6 |

Failed sign-ins, MFA codes and reset requests are also throttled per account and per IP in
MongoDB. Sign-in failures count per account from one client (5 per 15 minutes; a client is the
IPv4 address or the IPv6 /64), so a guesser locks only their own client out, and per account across
all clients (50 per 15 minutes): reaching that ceiling locks the account for everyone until the
window ends. To lift a sign-in lock early, clear the counters in `mongosh`:
`db.loginThrottle.deleteMany({Id: /^login:/})` (only counters; nothing else is affected). The
in-process limits reset on restart. The values are compiled-in defaults.

**Reverse proxies.** Per-IP limits use the connecting address unless it is a configured trusted
proxy (`OPEN_KASE_TRUSTED_PROXIES`), in which case `X-Forwarded-For` is used. A request with
`X-Forwarded-For` from an undeclared loopback or private address is treated as coming through an
undeclared proxy and gets no per-IP key, so all such requests count as one client. Declare your
proxy so per-IP and per-client limits work. Clients on public addresses cannot opt out by sending
the header.

## Capacity

Measured numbers and their conditions are in [Sizing and limits in `architecture.md`](architecture.md#sizing-and-limits). MongoDB memory is
the main sizing factor: the working set of tickets, messages and indexes should fit in the
WiredTiger cache.

## Failure behaviour

| Failure | Behaviour |
| --- | --- |
| Redis down | Ready stays 200 (`redis: degraded`); sign-in, tickets and replies work; rate limits still apply; live-update hints fall back to the 2 s outbox poll; job wake-ups fall back to polling |
| SMTP down | Ticket operations succeed; email deliveries are recorded and retried with backoff, then `failed` and retryable by hand |
| MongoDB down | Not ready (503); requests fail; nothing committed is lost; workers resume from leases when it returns |
| Application crash | systemd or Docker restarts it; in-flight requests fail; jobs whose lease expired are retried, at least once |
