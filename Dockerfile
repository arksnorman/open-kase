# open-kase: one image that serves the API and the built web UI.
#
#   docker build -t open-kase --build-arg VERSION=0.1.0 .
#
# The build stages run on the build host's architecture and cross-compile, so a
# multi-arch build (linux/amd64, linux/arm64) needs no emulation.

# ---- web UI ----
FROM --platform=$BUILDPLATFORM node:24.21.0-alpine AS ui
WORKDIR /src
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# ---- API binary ----
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS api
WORKDIR /src
COPY api/go.mod api/go.sum ./
RUN go mod download && go mod verify
COPY api/ ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/arksnorman/open-kase/api/package/product.Version=${VERSION} -X github.com/arksnorman/open-kase/api/package/product.Commit=${COMMIT}" \
    -o /out/open-kase .

# ---- runtime ----
FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.title="open-kase" \
      org.opencontainers.image.source="https://github.com/arksnorman/open-kase" \
      org.opencontainers.image.description="Self-hosted support ticket system"
COPY --from=api /out/open-kase /usr/local/bin/open-kase
COPY --from=ui /src/dist /srv/ui
USER nonroot:nonroot
EXPOSE 8080
ENV APP_ENV=production \
    OPEN_KASE_HTTP_ADDR=0.0.0.0:8080 \
    OPEN_KASE_MASTER_KEY_FILE=/run/secrets/master_key
HEALTHCHECK --interval=15s --timeout=6s --start-period=20s --retries=5 \
    CMD ["/usr/local/bin/open-kase", "health"]
ENTRYPOINT ["/usr/local/bin/open-kase"]
CMD ["serve", "--ui-dir", "/srv/ui"]
