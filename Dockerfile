# syntax=docker/dockerfile:1
# Multi-platform build: linux/amd64 and linux/arm64 (Raspberry Pi 4+).
# Build via the Makefile: make docker (local) or make docker-push (push to registry).

FROM --platform=${BUILDPLATFORM} golang:1.25.11-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILDDATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILDDATE} -s -w" \
    -o /out/nomad-gitops \
    ./cmd/nomad-gitops

# ── runtime image ──────────────────────────────────────────────────────────────
FROM alpine:3.24

LABEL org.opencontainers.image.title="nomad-gitops" \
      org.opencontainers.image.source="https://github.com/gerrowadat/nomad-gitops" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S nomad-gitops && \
    adduser -S -G nomad-gitops nomad-gitops

COPY --from=builder /out/nomad-gitops /usr/local/bin/nomad-gitops

USER nomad-gitops

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/nomad-gitops"]
