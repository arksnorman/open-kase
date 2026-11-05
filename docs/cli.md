# open-kase command line

One binary, `open-kase`, carries two kinds of commands:

- **Operator commands** (`init`, `serve`, `worker`, `doctor`, `jobs`, `seed-demo`, `keys`, `openapi`,
  `health`, `version`) run on the server host. They read the bootstrap environment (`APP_ENV`, `MONGODB_URI`,
  `REDIS_URI`, `OPEN_KASE_MASTER_KEY_FILE`; also `OPEN_KASE_HTTP_ADDR`, `OPEN_KASE_LOG_LEVEL`, `OPEN_KASE_LOG_FORMAT`,
  `OPEN_KASE_TRUSTED_PROXIES`, `OPEN_KASE_UI_DIR`) and act on the database directly. Mutating ones are audited as
  the operator (`cli <command> (os user <name>)`).
- **User commands** (`login`, `logout`, `tickets`, `settings`) run anywhere. They call a running
  server's REST API (`/api/v1`) with the user's own credentials, so they never need a MongoDB URI and can do
  exactly what the user could do in the web app — no more.

Run `open-kase help` for the list and `open-kase <command> --help` for flags.

## Signing in

```bash
open-kase login --url https://support.example.com            # prompts for email or username, hidden password, MFA code
open-kase login --url https://support.example.com --login dana --password-stdin < secret.txt
open-kase login --url https://support.example.com --login dana@example.com --password-stdin < secret.txt
printf '%s\n' "$OPEN_KASE_TOKEN" | open-kase login --url https://support.example.com --token-stdin
```

- `--login` takes an email address or a username (an identifier containing `@` is an email address).
- `--url` is required. Plain `http://` is accepted only for loopback hosts (`localhost`, `127.0.0.1`, `::1`);
  use `--allow-http` to accept it for another host on a trusted network.
- Passwords are never taken from arguments. Interactive use reads them with a hidden prompt; non-interactive
  use passes `--password-stdin`: line 1 is the password and, if the account uses MFA, line 2 is the 6-digit
  code or a recovery code (`abcd-efgh-ijkl`). Without a terminal and without `--password-stdin` the command
  exits with code 2 instead of waiting.
- `--token-stdin` stores an **API token** (created by an admin under Integrations) instead of a user session;
  it is verified with one API call (`GET /project`) before it is saved. This is the preferred mode for scripts
  and CI: the token is limited to its scopes, and it does not rotate. What it can do depends on its scopes: a
  token with `tickets:comment` replies as a bot under its own name, `tickets:internal` allows
  `tickets reply --audience agent`, and a token without a staff scope works as the requester side (public replies
  only, as the requester).
- When an administrator must enrol MFA (`auth.requireAdminMfa`), login succeeds but prints a warning; enrol in
  the web app first, because every other API call is refused with `mfaEnrolmentRequired`.

### Where credentials are stored

`$OPEN_KASE_CLI_CONFIG`, else `--config FILE`, else `<user config dir>/open-kase/credentials.json`
(`~/.config/open-kase/credentials.json` on Linux, `~/Library/Application Support/open-kase/…` on macOS).
The directory is created `0700` and the file is written atomically with mode `0600`; a file readable by other
users is refused. An OS keychain is not used yet.

A session file holds the same values the browser keeps in cookies — the rotating refresh token and the CSRF
token — plus the current access token (10 minutes). The CLI refreshes through `POST /auth/refresh` (sending the
refresh cookie, `X-CSRF-Token` and the server's own `Origin`) when the access token is about to expire, and
writes the rotated refresh token back. Refreshes are serialized with a lock file
(`credentials.json.lock`) because presenting one refresh token twice is treated as token theft and revokes the
session (the server tolerates a second presentation only for 10 seconds, and answers it without a new refresh
token, so the stored token must always come from the request that rotated it). Sessions end after the installation's absolute session lifetime (default 14 days), on logout, on a
password change or reset, or when an administrator disables the user; then commands exit with code 3.

`open-kase logout` revokes the server session (`POST /auth/logout`) and deletes the file. For an integration
token it only deletes the file — the token stays valid until an admin revokes it.

## Commands

| Command | API calls |
| --- | --- |
| `tickets list [--view VIEW] [--group NAME\|ID] [--limit N] [--json]` | `GET /groups` (to resolve a group name), `GET /tickets?view=&groupId=` |
| `tickets show NUMBER [--json]` | `GET /tickets/{t}`, `GET /tickets/{t}/messages` |
| `tickets claim NUMBER [--json]` | `GET /tickets/{t}` (version), `POST /tickets/{t}/claim` with `If-Match` |
| `tickets reply NUMBER --file reply.md --audience public\|agent [--json]` | `POST /tickets/{t}/messages` |
| `settings get smtp --redacted [--json]` | `GET /settings/smtp` (administrators) |

Notes:

- `tickets show|claim|reply` take a ticket number such as `1042` or `#1042` (one project per
  deployment, so there is no project argument). Output shows `#1042`.
- `--view` takes the API's queue views (`all`, `myWork` = Assigned to me, `unassigned`, `watching`, `awaitingCustomer`, `onHold`,
  `dueSoon`, `resolved`, `archived`). API tokens can use only `all`, `awaitingCustomer`, `onHold`, `resolved` and
  `archived`. `--limit` defaults to 50 (max 1000); pages are followed automatically.
- `tickets reply` requires `--audience`; it is never inferred. `public` sends a reply to the requester, `agent`
  adds a staff-only note. `--file -` reads the Markdown body from standard input.
- `tickets claim` sends the version it just read; if someone else changed the ticket in between, the server
  answers `409` and the command exits with code 5 — run it again after checking the ticket.
- `settings get smtp` always prints redacted values (`password: configured (redacted)`); the API never returns
  SMTP secrets, so `--redacted=false` is refused.
- `--json` prints the API's JSON (lists as an array of the items).

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Server, network or unexpected failure |
| 2 | Usage error: bad or missing flags or arguments, missing input, unreadable file |
| 3 | Not signed in, session ended, or credentials rejected (HTTP 401) |
| 4 | Forbidden or not found (HTTP 403/404) |
| 5 | Validation, conflict or precondition failure (HTTP 400/409/410/412/422/428) |
| 6 | Rate limited (HTTP 429; the message includes `Retry-After`) |
| 130 | Interrupted (Ctrl-C); in-flight requests are cancelled |

Errors are printed to standard error as `open-kase: <detail> (<code>, HTTP <status>)` with field errors on
following lines. Standard output carries only results, so `--json` output can be piped.

## Operator commands

`serve`, `init`, `worker`, `doctor`, `jobs`, `seed-demo` and `keys` accept the bootstrap flags `--master-key-file`,
`--log-level` and `--log-format`; `serve` also takes `--http-addr`, `--trusted-proxies` and `--ui-dir`. `health`,
`version` and `openapi` take only their own flags and need no database or master key. Exit codes are 0 (ok), 1 (runtime or dependency failure) and 2 (usage or
bootstrap configuration error).

| Command | Purpose |
| --- | --- |
| `init` | Idempotent installation: creates the master key file (mode 0600) if missing, collections and indexes (`EnsureSchema`), default settings, built-in statuses and request types, the single project with its Triage group, seed hold reasons and the first administrator. Flags: `--workspace-name`, `--admin-email`, `--admin-given-name`, `--admin-middle-name`, `--admin-family-name`, `--password-stdin` (the password is never a flag; without it a hidden prompt is used) |
| `serve` | HTTP API, outbox dispatcher and a bounded job pool. `--workers=false` serves HTTP only; `--worker-concurrency N` (1-64, default 4) |
| `worker` | Outbox dispatcher and job workers without HTTP (`--concurrency N`). Jobs are leased in MongoDB, so several workers never run the same attempt |
| `doctor` | Checks bootstrap configuration, master key, MongoDB (replica set), schema, installation, settings and Redis; prints `OK`, `INFO` or `FAIL` lines |
| `jobs list [--state queued\|running\|succeeded\|dead] [--kind KIND] [--limit N] [--json]`, `jobs retry ID` | Inspect durable jobs, requeue a dead one |
| `seed-demo` | Demo groups, agents, contacts and tickets, with notifications suppressed. Refused with `APP_ENV=production` unless `--force`; `--agent-password-stdin` reads the demo agents' password (default: generated) |
| `openapi --out FILE` | Writes the OpenAPI document (`-` for standard output); needs no database |
| `health [--url URL]` | Exits 0 when the readiness endpoint (default `http://127.0.0.1:8080/health/ready`) answers 200; for container health checks; needs no database |
| `version [--json]` | Product, version, commit, build time, Go version and platform |
| `keys rotate-jwt` | See below |

## Operator: rotating the JWT signing key

```bash
open-kase keys rotate-jwt [--overlap 15m] [--json]
```

Run on the server host (needs `MONGODB_URI` and the master key file). It creates a new active Ed25519 signing
key and retires the previous one. Every API process re-reads the active key at most once a minute, and the
retired key keeps **verifying** existing access tokens until `now + access-token lifetime + 2 minutes` (the
default and minimum `--overlap`), so nobody is signed out. After that the retired key verifies nothing.
Refresh tokens are opaque and unaffected. The rotation is audited as `signingKey.rotated`. Rotate after a
suspected key exposure or on a schedule; the private keys stay sealed with the master key.

