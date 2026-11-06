# open-kase

open-kase is a self-hostable support ticket system. One deployment runs one support desk: agents
work tickets from group queues, requesters are contacts, and external systems (mail gateways,
monitoring, your own customer portal, AI agents) create and update tickets through a versioned
REST API and an MCP endpoint with scoped API tokens. There are no customer logins and no built-in
AI; notifications go out by email and webhooks only.

## Features

- **One project per deployment, groups as team queues.** Tickets are numbered `#1`, `#2`, and so
  on, belong to one group, and are claimed atomically by its agents. Roles are admin and agent.
- **Tickets.** Priorities, a built-in Open / Awaiting customer / On hold / Resolved / Archived
  lifecycle, public replies and agent-only notes, attachments, watchers, scheduled send, tags
  (managed by admins), canned replies (text only), full-text search and an audit trail. Tickets
  are never deleted; archive is the terminal state.
- **SLA.** First-response targets, 24/7, one SLA.
- **Lifecycle automation.** Follow-up reminders, auto-resolve and auto-archive.
- **Reports.**
- **Notifications.** Email (SMTP) and outbound webhooks (per-endpoint custom headers, None, Basic
  or Token auth, optional custom CA and client certificate), delivered at least once from a
  durable outbox.
- **API and MCP.** REST under `/api/v1` and MCP at `/mcp`, with API tokens whose scopes the
  creator picks (`tickets:*`, `contacts:*`, `organisations:*`, `tags:*`, `holdReasons:*`). Every token write is audited.
  Building a customer portal on top: [`docs/customer-portal.md`](docs/customer-portal.md).
- **Security.** Server-side authorization on every route, short-lived JWTs with rotating refresh
  cookies, MFA, secrets encrypted under a master key kept outside the database, rate limits, and
  SSRF-guarded outbound calls.

## Quick start

With Docker Compose (MongoDB 8 as a single-node replica set, Redis and the published image
`ghcr.io/arksnorman/open-kase`):

```sh
mkdir open-kase && cd open-kase
mkdir secrets && openssl rand -base64 32 > secrets/master.key
sudo chown 65532:65532 secrets/master.key && chmod 600 secrets/master.key
# put compose.yaml and .env.example from this repository here, create .env from .env.example (passwords, SITE_NAME,
# COMPOSE_PROFILES=nginx or caddy for the bundled TLS proxy on ports 80/443), then:
docker compose up -d mongo redis
docker compose run --rm open-kase init      # creates the first administrator
docker compose up -d
```

The root `compose.yaml` (with the bundled nginx or Caddy TLS proxy), systemd installation and upgrade steps are in
[`docs/install.md`](docs/install.md). A single host gives you transactions and backups; it is not
a high-availability setup.

## Documentation

| Topic | Where |
| --- | --- |
| Install, configuration, upgrades | [`docs/install.md`](docs/install.md) |
| Backup, restore, health, logs, keys, SMTP | [`docs/operations.md`](docs/operations.md) |
| Command line | [`docs/cli.md`](docs/cli.md) |
| REST API | [`docs/api-contract-v1.md`](docs/api-contract-v1.md), [`docs/openapi.json`](docs/openapi.json) (also served at `/api/v1/openapi.json`) |
| MCP and webhooks | [`docs/mcp.md`](docs/mcp.md), [`docs/webhooks.md`](docs/webhooks.md) |
| Customer portal integration | [`docs/customer-portal.md`](docs/customer-portal.md) |
| Architecture and decisions | [`docs/architecture.md`](docs/architecture.md), [`docs/decisions.md`](docs/decisions.md) |
| Performance | [Sizing and limits](docs/architecture.md#sizing-and-limits) |
| Dependencies and licenses | [`docs/dependencies.md`](docs/dependencies.md) |

## Tech stack

Go (Gin, official MongoDB driver) in `api/`, Vue 3 with TypeScript, Vite, Pinia and Tailwind CSS in
`ui/`, MongoDB as a single-node replica set for transactions, and Redis for wake-ups and presence
only. The image is one Go binary that serves both the API and the built UI.

## Development

```sh
cd api && go build ./... && go test ./...   # MongoDB-backed tests skip without OPEN_KASE_TEST_MONGO_URI
cd ui && npm ci && npm run build
```

For the MongoDB-backed tests, start a local `mongod --replSet rs0`, run `rs.initiate()` once and set
`OPEN_KASE_TEST_MONGO_URI=mongodb://127.0.0.1:27017/?replicaSet=rs0`; `make test-integration` in `api/`
uses that URI by default. CI (`.github/workflows/ci.yml`) runs the full suite, and
`.github/workflows/release.yml` publishes the image to GHCR on `v*` tags (`latest`, version) and on
pushes to `main` (`edge`).

## License

open-kase is licensed under the [Apache License 2.0](LICENSE).
