# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
FROM golang:1.25-alpine AS build

ARG VERSION=dev
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
        -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Version=${VERSION} \
        -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Commit=${COMMIT} \
        -X github.com/ricgrangeia/server-space-manager-ai/internal/version.Date=${BUILD_DATE}" \
    -o /out/ssm ./cmd/ssm

# ---- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="server-space-manager-ai"
LABEL org.opencontainers.image.description="Lightweight Go daemon that tracks Docker and host disk usage and answers questions via a local LLM."
LABEL org.opencontainers.image.source="https://github.com/ricgrangeia/server-space-manager-ai"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/ssm /usr/local/bin/ssm

# /var/lib/ssm holds the SQLite DB, /etc/ssm holds the config.
USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ssm"]
