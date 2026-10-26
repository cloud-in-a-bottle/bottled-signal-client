FROM docker.io/library/golang:1.27-trixie AS build

WORKDIR /src
# The module and build caches persist between builds on the same host (podman keeps cache mounts), so a change to
# this app's code only recompiles its own packages, not mautrix and cgo sqlite.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# cgo for sqlite (mattn/go-sqlite3); sqlite_fts5 for message search.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags "-s -w" -o /out/bottled-signal-client ./cmd/bottled-signal-client

FROM docker.io/library/debian:trixie-slim

# signal-cli's native (GraalVM) build: a single binary needing only glibc >= 2.34 and zlib.
ARG SIGNAL_CLI_VERSION=0.14.9
ARG SIGNAL_CLI_SHA256=a8fab78b5e8053abf3db5725272957ad11d1745b9a16848ddb2ace4743dfa5d5
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
    && curl -fsSL -o /tmp/signal-cli.tar.gz \
        "https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}/signal-cli-${SIGNAL_CLI_VERSION}-Linux-native.tar.gz" \
    && echo "${SIGNAL_CLI_SHA256}  /tmp/signal-cli.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/signal-cli.tar.gz -C /usr/local/bin signal-cli \
    && rm /tmp/signal-cli.tar.gz \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
ENV SIGNAL_CLI_BIN=/usr/local/bin/signal-cli
COPY --from=build /out/bottled-signal-client /usr/local/bin/bottled-signal-client

EXPOSE 8080

CMD ["bottled-signal-client"]
