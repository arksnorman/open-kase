# Dependencies and licenses

Inventory for open-kase 0.1.0, from `api/go.mod` / `go list -m all` (licenses read from each
module's LICENSE file in the module cache) and from `ui/package.json` with the installed `ui/node_modules` (the `license`
field of each package). Versions are pinned: Go modules by `go.sum`, npm packages by exact versions and `ui/package-lock.json`.
Container images and native tools are pinned in the root `Dockerfile` and the compose file in `docs/install.md`.

No dependency uses a copyleft license that applies to the distributed binary. MPL-2.0 appears only as one option of
DOMPurify's dual license (MPL-2.0 OR Apache-2.0). The IBM Plex fonts are under the SIL Open Font License 1.1.

Email uses `github.com/wneessen/go-mail` (not the older Gomail package). Regenerate after dependency changes; `govulncheck ./...` (in `api/`) and `npm audit` (in `ui/`) are the vulnerability checks.

## Go modules compiled into the `open-kase` binary

| Module | Version | Direct | License | Used for |
| --- | --- | --- | --- | --- |
| `github.com/gin-gonic/gin` | v1.12.0 | yes | MIT | HTTP router |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | yes | MIT | JWT access tokens (EdDSA) |
| `github.com/google/jsonschema-go` | v0.4.3 | yes | MIT | MCP tool schemas |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | yes | Apache-2.0 | MCP Streamable HTTP server (official SDK) |
| `github.com/redis/go-redis/v9` | v9.22.0 | yes | BSD-2-Clause | Redis client (wake-ups, presence hints) |
| `github.com/wneessen/go-mail` | v0.8.1 | yes | MIT | Email: the SMTP adapter behind the `Mailer` interface (`api/internal/mailer`) |
| `go.mongodb.org/mongo-driver/v2` | v2.9.1 | yes | Apache-2.0 | MongoDB driver (official) |
| `golang.org/x/crypto` | v0.58.0 | yes | BSD-3-Clause | Argon2id, Ed25519 |
| `golang.org/x/term` | v0.47.0 | yes | BSD-3-Clause | hidden password prompts (CLI) |
| `github.com/cespare/xxhash/v2` | v2.3.0 |  | MIT |  |
| `github.com/gabriel-vasile/mimetype` | v1.4.12 |  | MIT |  |
| `github.com/gin-contrib/sse` | v1.1.0 |  | MIT |  |
| `github.com/go-playground/locales` | v0.14.1 |  | MIT |  |
| `github.com/go-playground/universal-translator` | v0.18.1 |  | MIT |  |
| `github.com/go-playground/validator/v10` | v10.30.1 |  | MIT |  |
| `github.com/goccy/go-yaml` | v1.19.2 |  | MIT |  |
| `github.com/klauspost/compress` | v1.19.2 |  | Apache-2.0 |  |
| `github.com/leodido/go-urn` | v1.4.0 |  | MIT |  |
| `github.com/mattn/go-isatty` | v0.0.20 |  | MIT |  |
| `github.com/pelletier/go-toml/v2` | v2.2.4 |  | MIT |  |
| `github.com/quic-go/qpack` | v0.6.0 |  | MIT |  |
| `github.com/quic-go/quic-go` | v0.59.1 |  | MIT |  |
| `github.com/segmentio/asm` | v1.1.3 |  | MIT |  |
| `github.com/segmentio/encoding` | v0.5.4 |  | MIT |  |
| `github.com/ugorji/go/codec` | v1.3.1 |  | MIT |  |
| `github.com/xdg-go/scram` | v1.2.0 |  | Apache-2.0 |  |
| `github.com/xdg-go/stringprep` | v1.0.4 |  | Apache-2.0 |  |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 |  | BSD-3-Clause |  |
| `github.com/youmark/pkcs8` | v0.0.0-20240726163527-a2c0da244d78 |  | MIT |  |
| `go.uber.org/atomic` | v1.11.0 |  | MIT |  |
| `golang.org/x/net` | v0.62.0 |  | BSD-3-Clause |  |
| `golang.org/x/oauth2` | v0.35.0 |  | BSD-3-Clause |  |
| `golang.org/x/sync` | v0.24.0 |  | BSD-3-Clause |  |
| `golang.org/x/sys` | v0.49.0 |  | BSD-3-Clause |  |
| `golang.org/x/text` | v0.43.0 |  | BSD-3-Clause |  |
| `golang.org/x/time` | v0.15.0 |  | BSD-3-Clause |  |
| `google.golang.org/protobuf` | v1.36.10 |  | BSD-3-Clause |  |

The module graph lists further modules (test-only dependencies of dependencies, tooling) that are not compiled
into the binary;
`go list -deps ./...` is the authoritative set above.

Toolchain: Go 1.27.3 (BSD-3-Clause).

## npm packages (ui/, top level)

| Package | Version | Kind | License |
| --- | --- | --- | --- |
| `@floating-ui/dom` | 1.8.0 | runtime | MIT |
| `@fontsource/ibm-plex-mono` | 5.3.0 | runtime | OFL-1.1 |
| `@fontsource/ibm-plex-sans` | 5.3.0 | runtime | OFL-1.1 |
| `@tabler/icons-vue` | 3.48.0 | runtime | MIT |
| `@tiptap/core` | 3.31.4 | runtime | MIT |
| `@tiptap/extensions` | 3.31.4 | runtime | MIT |
| `@tiptap/markdown` | 3.31.4 | runtime | MIT |
| `@tiptap/pm` | 3.31.4 | runtime | MIT |
| `@tiptap/starter-kit` | 3.31.4 | runtime | MIT |
| `@tiptap/vue-3` | 3.31.4 | runtime | MIT |
| `dompurify` | 3.4.17 | runtime | (MPL-2.0 OR Apache-2.0) |
| `markdown-it` | 15.0.3 | runtime | MIT |
| `pinia` | 4.0.3 | runtime | MIT |
| `qrcode` | 1.5.4 | runtime | MIT |
| `vue` | 3.5.43 | runtime | MIT |
| `vue-router` | 5.3.1 | runtime | MIT |
| `@tailwindcss/vite` | 4.3.3 | build | MIT |
| `@types/node` | 24.19.1 | build | MIT |
| `@types/qrcode` | 1.5.6 | build | MIT |
| `@vitejs/plugin-vue` | 6.0.9 | build | MIT |
| `@vue/tsconfig` | 0.9.1 | build | MIT |
| `openapi-typescript` | 7.13.0 | build | MIT |
| `tailwindcss` | 4.3.3 | build | MIT |
| `typescript` | 6.0.3 | build | Apache-2.0 |
| `vite` | 8.3.5 | build | MIT |
| `vue-tsc` | 3.3.11 | build | MIT |

Runtime packages end up in the built SPA bundle; build packages are used only to compile it. Transitive npm
dependencies are pinned in `ui/package-lock.json` and not listed here.

## Infrastructure (not linked)

| Component | Version | License |
| --- | --- | --- |
| MongoDB Community Server | 8.0.32 | SSPL-1.0 (run as a separate service, not distributed with open-kase) |
| MongoDB Database Tools (`mongodump`, `mongorestore`) | 100.19.1 | Apache-2.0 |
| mongosh | 2.12.0 | Apache-2.0 |
| Redis (Ubuntu 24.04 package on the dev host) | 7.0.15 | BSD-3-Clause |
| Redis image (Docker Compose) | 8.2.2-alpine | RSALv2 / SSPLv1 / AGPLv3 (tri-license; run unmodified as a service) |
| Base images | `golang:1.27.3-trixie` (build), `gcr.io/distroless/static-debian13:nonroot`, `node:24.21.0-alpine` (build) | various (Debian/Alpine) |

The SPDX identifiers above were detected automatically; check the upstream LICENSE files before relying on them
for a compliance decision.
