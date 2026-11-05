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
`http://localhost`).

## Configuration

Bootstrap configuration is small and comes from the environment (or the matching flag). Everything
else (base URL, SMTP, limits, token lifetimes) is stored in MongoDB and edited in the UI under
Settings → Configuration.

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

Requires Docker with Compose v2. MongoDB shares the host's Linux kernel, and MongoDB 8.0 refuses to
start on kernels 6.19 to 7.0.13 because of a kernel bug (see
[SERVER-125742](https://jira.mongodb.org/browse/SERVER-125742)); check `uname -r` and use a kernel
older than 6.19 or 7.0.14 and newer. Pick a directory and create the master key:

```sh
mkdir open-kase && cd open-kase
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
# Compose reads this file for the connection string below (hex, so no URL escaping needed):
echo "MONGO_PASSWORD=$(cat secrets/mongo_root_password)" > .env
# Redis requires a password too (hex again, so it can sit in REDIS_URI unescaped):
openssl rand -hex 32 > secrets/redis_password
echo "REDIS_PASSWORD=$(cat secrets/redis_password)" >> .env
chmod 600 secrets/redis_password .env
```

Save this as `compose.yaml`:

```yaml
name: open-kase

services:
  mongo:
    image: mongo:8.0.34
    restart: unless-stopped
    # Install the keyfile with the ownership and mode mongod requires, then
    # hand over to the official entrypoint.
    entrypoint:
      - bash
      - -c
      - install -m 0400 -o mongodb -g mongodb /run/secrets/mongo_keyfile /tmp/mongo-keyfile && exec docker-entrypoint.sh "$$@"
      - --
    command: ["mongod", "--replSet", "rs0", "--bind_ip_all", "--keyFile", "/tmp/mongo-keyfile"]
    environment:
      MONGO_INITDB_ROOT_USERNAME: root
      MONGO_INITDB_ROOT_PASSWORD_FILE: /run/secrets/mongo_root_password
    secrets: [mongo_root_password, mongo_keyfile]
    volumes:
      - mongo-data:/data/db
    # Initiates the single-member replica set on first start, then reports
    # healthy once there is a primary. No port is published.
    healthcheck:
      test:
        - CMD-SHELL
        - >-
          mongosh --quiet -u root -p "$$(cat /run/secrets/mongo_root_password)" --authenticationDatabase admin --eval
          "try { rs.status().ok } catch (e) { rs.initiate({_id: 'rs0', members: [{_id: 0, host: 'mongo:27017'}]}).ok }; db.hello().isWritablePrimary"
          | grep -q true
      interval: 10s
      timeout: 10s
      retries: 12
      start_period: 20s
    stop_grace_period: 60s

  redis:
    image: redis:8.2.2-alpine
    restart: unless-stopped
    # Cache and presence only; the data is rebuildable, so persistence is off.
    # The password comes from a secret file, so it does not show in the process list.
    command:
      - sh
      - -c
      - exec docker-entrypoint.sh redis-server --requirepass "$$(cat /run/secrets/redis_password)" --save "" --appendonly no --maxmemory 256mb --maxmemory-policy allkeys-lru
    secrets: [redis_password]
    healthcheck:
      test: ["CMD-SHELL", 'REDISCLI_AUTH="$$(cat /run/secrets/redis_password)" redis-cli ping | grep -q PONG']
      interval: 10s
      timeout: 5s
      retries: 6

  open-kase:
    image: ghcr.io/arksnorman/open-kase:0.1.0   # pinned; bump it when you upgrade
    restart: unless-stopped
    environment:
      APP_ENV: production
      MONGODB_URI: mongodb://root:${MONGO_PASSWORD}@mongo:27017/open_kase?replicaSet=rs0&authSource=admin
      REDIS_URI: redis://:${REDIS_PASSWORD}@redis:6379/0
      OPEN_KASE_MASTER_KEY_FILE: /run/secrets/master_key
      # The reverse proxy reaches the app through the Docker bridge network
      # (default pools are inside 172.16.0.0/12). The port below is published
      # on loopback only, so only this host can send X-Forwarded-For.
      OPEN_KASE_TRUSTED_PROXIES: 172.16.0.0/12
    secrets: [master_key]
    depends_on:
      mongo: { condition: service_healthy }
      redis: { condition: service_healthy }
    ports:
      - "127.0.0.1:8080:8080"
    stop_grace_period: 30s

secrets:
  master_key:
    file: ./secrets/master.key
  mongo_root_password:
    file: ./secrets/mongo_root_password
  mongo_keyfile:
    file: ./secrets/mongo_keyfile
  redis_password:
    file: ./secrets/redis_password

volumes:
  mongo-data:
```

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

The application is now on `127.0.0.1:8080`, and the image's health check (`open-kase health`)
shows in `docker compose ps`. Continue with the reverse proxy below. The compose file pins the
release (`:0.1.0`); to upgrade, change the tag to the new version and run `docker compose pull &&
docker compose up -d`. `latest` follows the newest release tag and `edge` is the head of `main`
(published only after CI passed); neither is meant for production.

Pushes to `main` and `v*` tags run the CI checks first (the release workflow calls them); the image
is only built and pushed when they pass. The image is first published when a `v*` tag is pushed (`:latest`, `:<version>` and
`:<major>.<minor>`); until then only `:edge` exists, built from `main`. After the first publish the
owner must set the GHCR package's visibility to public (GitHub → Packages → open-kase → Package
settings), otherwise users must run `docker login ghcr.io` with a token that has `read:packages`.

## Reverse proxy and TLS

Run Caddy (or nginx) on the host, listening on 80/443 and proxying to `127.0.0.1:8080`. Caddy gets
and renews certificates by itself and forwards `Host` and `X-Forwarded-For` unchanged:

```
# /etc/caddy/Caddyfile
support.example.com {
	encode zstd gzip
	header Strict-Transport-Security "max-age=31536000"
	request_body {
		max_size 60MB
	}
	reverse_proxy 127.0.0.1:8080
}
```

With nginx:

```nginx
server {
    listen 443 ssl;
    server_name support.example.com;
    # ssl_certificate / ssl_certificate_key (for example from certbot)
    client_max_body_size 60m;          # attachments are limited by the app (10 MiB by default)
    add_header Strict-Transport-Security "max-age=31536000" always;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;               # keep the port: the API compares Origin with Host
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;                            # live-update streams
        proxy_read_timeout 120s;
    }
}
```

`OPEN_KASE_TRUSTED_PROXIES` must cover the address the proxy connects from. With the compose file
above that is the Docker bridge gateway, which `172.16.0.0/12` includes; if your Docker uses other
address pools, adjust it. After the first sign-in, check that the access log shows your real
address (`docker compose logs open-kase`, field `clientIp`). If the proxy runs on another machine,
change the port mapping to an address it can reach and list that proxy's address instead.

Then open `https://support.example.com/` and set **Settings → Configuration → General → base URL**
to that address; email links need it.

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
