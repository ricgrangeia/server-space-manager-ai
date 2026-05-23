# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
FROM golang:1.25-alpine AS build

# Version lives in source (internal/version.Version). Commit and BUILD_DATE
# are optional provenance — pass via --build-arg in a release workflow if you
# want them baked in; otherwise they default to "none" / "unknown".
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src

# Cache module downloads in their own layer
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Static binary, version baked in. CGO disabled — we use modernc.org/sqlite
# (pure Go), so no libc dependency is required.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
        -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Commit=${COMMIT} \
        -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Date=${BUILD_DATE}" \
    -o /out/ssm ./cmd/ssm

# ---- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12

LABEL org.opencontainers.image.title="server-space-manager-ai"
LABEL org.opencontainers.image.description="Lightweight Go daemon that tracks Docker and host disk usage and answers questions via a local LLM."
LABEL org.opencontainers.image.source="https://github.com/ricgrangeia/server-space-manager-ai"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/ssm /usr/local/bin/ssm

# Bake a sensible default config into the image so the container starts
# even when no config.yaml is mounted from the host (e.g. Portainer git
# stacks where the file isn't checked in). Operators override individual
# values via SSM_* environment variables.
COPY config.example.yaml /etc/ssm/config.yaml

# Runs as root inside the container so it can read /var/run/docker.sock
# (typically owned by root:docker on the host). The container itself has
# only read-only mounts (/, the docker socket, the config) plus its own
# private named volume for SQLite — the writable surface is tiny and the
# binary issues GET-only requests against the Docker API.
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ssm"]
