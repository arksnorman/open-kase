# Installing open-kase

open-kase runs on a single host as three things: **MongoDB** (a single-node replica set, which
transactions require), **Redis** (cache and presence only) and the **open-kase** application,
which serves the API and the web UI from one process on one port. There is no high-availability
setup: when the host, MongoDB or the application is down, the helpdesk is down. Backups are in
[`operations.md`](operations.md).

Two ways to run it:

- [Docker Compose](#docker-compose) with the published image `ghcr.io/arksnorman/open-kase`
- [Native binary with systemd](#native-binary-with-systemd)

Either way, put a TLS-terminating [reverse proxy](#reverse-proxy-and-tls) in front: session
cookies are `Secure` outside `APP_ENV=development`, so browsers only keep them over HTTPS (or on
`http://localhost`). The Docker Compose setup below bundles one (nginx or Caddy, your choice).

## Configuration

Bootstrap configuration is small and comes from the environment (or the matching flag). Everything
else (base URL, SMTP, limits, token lifetimes) is stored in MongoDB and edited in the UI under
Settings → Configuration. Set the **base URL** (Settings → Configuration → General) to the public
address of the installation, for example `https://support.example.com`: email links are built from
it, and the API also accepts requests whose `Origin` matches it.

| Variable | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `APP_ENV` | | `production` | `production`, `development` or `test`. Production logs JSON and sets `Secure` cookies; only `development` disables them |
| `MONGODB_URI` | | none, required | MongoDB URI with the database name and `replicaSet=`, e.g. `mongodb://root:PASSWORD@mongo:27017/open_kase?replicaSet=rs0&authSource=admin` |
| `REDIS_URI` | | none, required by `serve` | e.g. `redis://:PASSWORD@redis:6379/0` (the password form of the URI is supported). Redis being down only degrades live updates |
| `OPEN_KASE_MASTER_KEY_FILE` | `--master-key-file` | `/etc/open-kase/master.key` | File with the 32-byte base64 master key that encrypts secrets stored in MongoDB |
| `OPEN_KASE_HTTP_ADDR` | `--http-addr` | `:8080` | Listen address (the image sets `0.0.0.0:8080`) |
| `OPEN_KASE_TRUSTED_PROXIES` | `--trusted-proxies` | empty | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For` is trusted |
| `OPEN_KASE_UI_DIR` | `--ui-dir` | empty | Directory with the built web UI to serve; the image passes `--ui-dir /srv/ui` |
| `OPEN_KASE_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `OPEN_KASE_LOG_FORMAT` | `--log-format` | `json` in production, else `text` | `json` or `text` |

Trusted proxies matter. Sign-in throttling is per client address. Behind a proxy that is not
declared, every request appears to come from the proxy, so five wrong passwords for an account
lock it for everyone for 15 minutes. List the proxy's address here so the real client address from
`X-Forwarded-For` is used. The proxy must also forward `Host` unchanged (including a non-default
port): the API only accepts cookie-authenticated requests whose `Origin` matches the `Host` or the
configured base URL.

**The master key is not in MongoDB.** Back it up separately from database backups
([`operations.md`](operations.md)). If it is lost, the encrypted settings (SMTP password, webhook
credentials, MFA secrets, signing keys) cannot be decrypted, and a different key makes the
application refuse to become ready.

## Docker Compose

Requires Docker with Compose v2.23 or newer (the proxy configuration is embedded in the
repository's `compose.yaml` with `configs: content:`, which older versions reject; check `docker compose version`).
MongoDB shares the host's Linux kernel, and MongoDB 8.0 refuses to
start on kernels 6.19 to 7.0.13 because of a kernel bug (see
[SERVER-125742](https://jira.mongodb.org/browse/SERVER-125742)); check `uname -r` and use a kernel
older than 6.19 or 7.0.14 and newer. Pick a directory and create the master key:

```sh
# Get the two files, either by cloning the repository ...
git clone https://github.com/arksnorman/open-kase.git open-kase && cd open-kase
# ... or by downloading them into an empty directory:
#   mkdir open-kase && cd open-kase
#   curl -fsSLO https://raw.githubusercontent.com/arksnorman/open-kase/v0.2.0/compose.yaml
#   curl -fsSLO https://raw.githubusercontent.com/arksnorman/open-kase/v0.2.0/.env.example
mkdir secrets && chmod 700 secrets
openssl rand -base64 32 > secrets/master.key
chmod 600 secrets/master.key
# The image runs as uid 65532 and must be able to read the key:
sudo chown 65532:65532 secrets/master.key

# MongoDB runs with authentication and a replica-set keyfile:
openssl rand -hex 32 > secrets/mongo_root_password
openssl rand -base64 756 > secrets/mongo_keyfile
chmod 600 secrets/mongo_keyfile
# mongod reads the password file after dropping to its own user, so the file must be
# world-readable; the secrets directory (mode 700) keeps it private on the host:
chmod 644 secrets/mongo_root_password
# Redis requires a password too (hex again, so it can sit in REDIS_URI unescaped):
openssl rand -hex 32 > secrets/redis_password
chmod 600 secrets/redis_password

# Compose settings, from .env.example:
cp .env.example .env
# Compose reads the passwords from .env for the connection strings below:
sed -i "s/^MONGO_PASSWORD=.*/MONGO_PASSWORD=$(cat secrets/mongo_root_password)/" .env
sed -i "s/^REDIS_PASSWORD=.*/REDIS_PASSWORD=$(cat secrets/redis_password)/" .env
sed -i "s/^SITE_NAME=.*/SITE_NAME=support.example.com/" .env    # your own host name
chmod 600 .env
```

The `.env` file (from [`.env.example`](../.env.example)) holds what `compose.yaml` reads:

| Variable | Default | Purpose |
| --- | --- | --- |
| `COMPOSE_PROFILES` | `nginx` | Which bundled TLS proxy runs: `nginx` or `caddy`; empty for your own proxy (see [Reverse proxy and TLS](#reverse-proxy-and-tls)) |
| `SITE_NAME` | `support.example.com` | Host name of the installation: goes into the self-signed certificate (nginx) or is the name Caddy gets a certificate for |
| `HTTP_PORT`, `HTTPS_PORT` | `80`, `443` | Host ports the proxy publishes |
| `OPEN_KASE_VERSION` | `0.2.0` | Image tag of `open-kase`; bump it to upgrade |
| `MONGO_PASSWORD` | none | Must equal `secrets/mongo_root_password` |
| `REDIS_PASSWORD` | none | Must equal `secrets/redis_password` |

The services in `compose.yaml` (read the file; every setting has a comment):

| Service | Profile | What it is |
| --- | --- | --- |
| `mongo` | always | `mongo:8.0.34` with authentication and a replica-set keyfile; initiates the single-member replica set `rs0` on first start; no published port |
| `redis` | always | `redis:8.2.2-alpine` with a password, no persistence; no published port |
| `open-kase` | always | `ghcr.io/arksnorman/open-kase:${OPEN_KASE_VERSION:-0.2.0}`: API, MCP and web UI on port 8080, no published port; trusts proxies in `172.16.0.0/12` |
| `nginx` | `nginx` | `nginx:1.30.6`, TLS on `HTTP_PORT`/`HTTPS_PORT`, proxies to `open-kase:8080`; configuration inline (`configs:`) |
| `caddy` | `caddy` | `caddy:2.11.8-alpine`, same role; configuration inline; data in the `caddy-data` and `caddy-config` volumes |

Run `docker compose config -q` to validate the file and your `.env`.

Start the databases, then initialize the installation once. `init` creates the collections and
indexes, the workspace, the single project with its Triage group, and the first administrator. It
asks for the workspace name, the administrator's email and name, and a password at a hidden prompt:

```sh
docker compose up -d mongo redis
docker compose run --rm open-kase init
docker compose up -d
docker compose run --rm open-kase doctor
```

For scripted installs `init` also takes `--workspace-name`, `--admin-email`, `--admin-given-name`,
`--admin-family-name` and `--password-stdin` (the password is never accepted as a flag).

The application is now running behind the proxy chosen in `COMPOSE_PROFILES`, and the image's
health check (`open-kase health`) shows in `docker compose ps`. Continue with the reverse proxy
below. The compose file pins the
release (`:0.2.0`); to upgrade, set `OPEN_KASE_VERSION` in `.env` to the new version and run `docker compose pull &&
docker compose up -d`. `latest` follows the newest release tag and `edge` is the head of `main`
(published only after CI passed); neither is meant for production.

Pushes to `main` and `v*` tags run the CI checks first (the release workflow calls them); the image
is only built and pushed when they pass. The image is first published when a `v*` tag is pushed (`:latest`, `:<version>` and
`:<major>.<minor>`); until then only `:edge` exists, built from `main`. After the first publish the
owner must set the GHCR package's visibility to public (GitHub → Packages → open-kase → Package
settings), otherwise users must run `docker login ghcr.io` with a token that has `read:packages`.

## Reverse proxy and TLS

The Compose file above includes two interchangeable TLS reverse proxies; `COMPOSE_PROFILES` in
`.env` picks one (`nginx` or `caddy`, never both: they use the same ports). Only the proxy
publishes ports (`HTTP_PORT`, default 80, and `HTTPS_PORT`, default 443); the `open-kase` service
publishes none. The proxy sends everything to `open-kase:8080` and serves no files itself: the UI
and its security headers (a CSP with an inline-script hash) come from the application, so the UI and
the API share one origin.

`OPEN_KASE_TRUSTED_PROXIES: 172.16.0.0/12` is the address range of the Compose bridge network that
the proxy connects from, so the real client address from `X-Forwarded-For` is used for sign-in
throttling and logging. It covers Docker's default pools; if your daemon uses others (the
`192.168.0.0/16` pool is used once the `172.x` ones are exhausted, or `default-address-pools` is
set), change it to match. After the first sign-in, check that the access log shows your real address
(`docker compose logs open-kase`, field `clientIp`).

After the first start, open `https://<SITE_NAME>/` and set **Settings → Configuration → General →
base URL** to that address; email links need it.

### nginx (`COMPOSE_PROFILES=nginx`)

Certificates live in `./certs` (`fullchain.pem`, `privkey.pem`), a read-write bind mount.

- With no certificate there, the container generates a self-signed one at start (EC P-256, 825 days,
  CN and subject alternative name from `SITE_NAME`, key mode 600) and writes it to `./certs`.
  Later restarts reuse it, so browsers see a stable certificate to trust. Delete the files to
  regenerate. The first browser visit shows a certificate warning.
- For a real certificate, put your files there before the first start (Let's Encrypt via certbot
  on the host, a corporate CA, and so on): `cp fullchain.pem privkey.pem certs/`, then
  `docker compose up -d` (or `docker compose restart nginx` after a renewal). The
  `Strict-Transport-Security` header is added automatically whenever the certificate is not
  self-signed.
- Port 80 redirects to HTTPS. With a non-default `HTTPS_PORT` the redirect includes that port, and
  `Host` is forwarded with its port (`$http_host`), which the API's `Origin` check needs.
- The proxy buffers nothing and allows idle responses for an hour, for the live-update stream
  (`/api/v1/events`) and MCP (`/mcp`); request bodies up to 60 MB pass (the app limits attachments).

The image is the Debian-based `nginx`, not `nginx:*-alpine`: neither alpine variant ships the
`openssl` binary, and installing it at start would need network access.

### Caddy (`COMPOSE_PROFILES=caddy`)

Caddy obtains certificates by itself and keeps them in the `caddy-data` volume.

- For a public domain name in `SITE_NAME`, it gets and renews a Let's Encrypt certificate. The
  name must resolve to this host and ports 80 and 443 must be reachable from the internet (so
  leave `HTTP_PORT` and `HTTPS_PORT` at their defaults).
- For `localhost` or an IP address it uses its own internal CA (browse to the exact name or IP in
  `SITE_NAME`: Caddy picks the certificate by name): a certificate nobody trusts until
  you import Caddy's root (`docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .`).
- To use your own certificate instead, uncomment the `./certs:/certs` volume and the `tls
  /certs/fullchain.pem /certs/privkey.pem` line of the Caddyfile in `compose.yaml`, put the files in
  `./certs` and run `docker compose up -d`. Add `header Strict-Transport-Security "max-age=31536000"`
  to the site block once you are sure about HTTPS.
- Live-update and MCP streams are flushed immediately (`flush_interval -1`), and request bodies are
  limited to 60 MB.

### Your own proxy instead

To run Caddy, nginx or another proxy on the host (or another machine), remove the profile
(`COMPOSE_PROFILES` empty or deleted in `.env`) and publish the app on loopback by adding this to
the `open-kase` service:

```yaml
    ports:
      - "127.0.0.1:8080:8080"
```

Then proxy to `127.0.0.1:8080`, forwarding `Host` unchanged (with its port, if not default) and
`X-Forwarded-For`, without buffering streamed responses, and set `OPEN_KASE_TRUSTED_PROXIES` to the
address the proxy connects from: for a proxy on the host, the bridge gateway (`172.16.0.0/12`
covers it); for another machine, that machine's address, with the port published on an address it
can reach. Caddy:

```
support.example.com {
	header Strict-Transport-Security "max-age=31536000"
	request_body {
		max_size 60MB
	}
	reverse_proxy 127.0.0.1:8080
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name support.example.com;
    # ssl_certificate / ssl_certificate_key (for example from certbot)
    client_max_body_size 60m;
    add_header Strict-Transport-Security "max-age=31536000" always;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

## Native binary with systemd

You need MongoDB 8 running as a single-node replica set (`replication.replSetName: rs0` in
`mongod.conf`, then `rs.initiate()` once in `mongosh`), Redis, and the `open-kase` binary and web
UI built from this repository (Go and Node versions are in `api/go.mod` and `ui/package.json`):

```sh
(cd ui && npm ci && npm run build)
make -C api build        # CGO_ENABLED=0 build with the version ldflags, writes api/bin/open-kase
sudo install -m 0755 api/bin/open-kase /usr/local/bin/open-kase
sudo install -d /opt/open-kase && sudo cp -r ui/dist /opt/open-kase/ui
```

Create the service user and the environment file, then initialize:

```sh
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin open-kase
sudo install -d -m 0750 -o root -g open-kase /etc/open-kase
sudo install -m 0640 -o root -g open-kase /dev/null /etc/open-kase/open-kase.env
sudoedit /etc/open-kase/open-kase.env        # see below
# init creates /etc/open-kase/master.key (mode 0600) when it does not exist:
sudo sh -c 'set -a; . /etc/open-kase/open-kase.env; open-kase init'
```

`/etc/open-kase/open-kase.env`:

```
APP_ENV=production
MONGODB_URI=mongodb://127.0.0.1:27017/open_kase?replicaSet=rs0   # add user:password@ and authSource= if your mongod requires auth
REDIS_URI=redis://127.0.0.1:6379/0
# Listen on loopback only when a reverse proxy runs on the same host, and trust it:
OPEN_KASE_HTTP_ADDR=127.0.0.1:8080
OPEN_KASE_TRUSTED_PROXIES=127.0.0.1,::1
# Serve the web UI from the same process (without it only the API is served):
OPEN_KASE_UI_DIR=/opt/open-kase/ui
```

`/etc/systemd/system/open-kase.service`:

```ini
[Unit]
Description=open-kase support ticket system
Documentation=https://github.com/arksnorman/open-kase
After=network-online.target mongod.service redis-server.service
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=open-kase
Group=open-kase
EnvironmentFile=/etc/open-kase/open-kase.env
# The key is handed to the service as a systemd credential, so the service
# user never needs read access to /etc/open-kase/master.key itself.
LoadCredential=master.key:/etc/open-kase/master.key
Environment=OPEN_KASE_MASTER_KEY_FILE=%d/master.key
ExecStart=/usr/local/bin/open-kase serve
Restart=on-failure
RestartSec=5s
TimeoutStopSec=45s
KillSignal=SIGTERM

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
UMask=0077

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now open-kase
journalctl -u open-kase -f
```

## First sign-in

Open the address of the installation and sign in with the administrator email and password you gave
to `init`. Administrators must enrol an authenticator app (TOTP) at first sign-in. A clean install
has exactly one project and one group, **Triage**; new tickets land there unassigned. Create the
other groups, add agents, and set the base URL and SMTP under Settings. Triage can be renamed but
not deactivated or deleted.

Notifications are sent once SMTP is configured; until then deliveries are recorded as skipped.
See [`operations.md`](operations.md#smtp).

## Upgrading

Docker Compose:

```sh
docker compose pull open-kase
docker compose up -d open-kase
docker compose run --rm open-kase doctor
```

Native: replace `/usr/local/bin/open-kase` and the UI directory, then `systemctl restart open-kase`.

There are no migrations. The application creates any missing collection, validator and index when
it starts, which is idempotent and cheap when everything is in place. Take a backup first
([`operations.md`](operations.md#backup-and-restore)); to roll back, restore it and run the older
version.
