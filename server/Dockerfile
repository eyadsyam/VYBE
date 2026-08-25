# Multi-stage. Two final targets:
#   tools    — migrate / seed / probe, run once and exit
#   runtime  — the API server
#
# docker-compose.yml selects between them, so the migration job and the API
# share one build cache rather than compiling the module twice.

# --- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build

# git: some modules resolve via VCS. ca-certificates: outbound TLS to the
# metadata provider.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the result runs on a scratch/alpine base with no libc surprises.
# -trimpath keeps absolute build paths out of the binary; ldflags -s -w drop
# the symbol table and DWARF, which is a meaningful size saving and removes
# information an attacker would otherwise get for free.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api     ./cmd/api     && \
    go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate && \
    go build -trimpath -ldflags="-s -w" -o /out/seed    ./cmd/seed    && \
    go build -trimpath -ldflags="-s -w" -o /out/probe   ./cmd/probe

# --- tools ------------------------------------------------------------------
FROM alpine:3.21 AS tools
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/migrate /out/seed /out/probe /app/
# Run-once jobs still drop privileges: a migration container is a database
# superuser session, which is exactly the thing not to run as root.
RUN adduser -D -u 10001 vybe
USER vybe

# --- runtime ----------------------------------------------------------------
FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/api /app/api

RUN adduser -D -u 10001 vybe
USER vybe

EXPOSE 8080

# The healthcheck runs the binary's own probe rather than shelling out to curl,
# so the image needs no HTTP client and the check exercises the same config
# loading the server does.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/app/api", "-healthcheck"]

ENTRYPOINT ["/app/api"]
