FROM python:3.12-slim

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# signal-cli native (GraalVM) build: a single static-ish binary needing only glibc >= 2.34 and zlib.
ARG SIGNAL_CLI_VERSION=0.14.9
ARG SIGNAL_CLI_SHA256=a8fab78b5e8053abf3db5725272957ad11d1745b9a16848ddb2ace4743dfa5d5
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL -o /tmp/signal-cli.tar.gz \
        "https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}/signal-cli-${SIGNAL_CLI_VERSION}-Linux-native.tar.gz" \
    && echo "${SIGNAL_CLI_SHA256}  /tmp/signal-cli.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/signal-cli.tar.gz -C /usr/local/bin signal-cli \
    && rm /tmp/signal-cli.tar.gz \
    && rm -rf /var/lib/apt/lists/*
ENV SIGNAL_CLI_BIN=/usr/local/bin/signal-cli

WORKDIR /app

# Install Python dependencies (source is copied first so the project itself
# builds during `uv sync`).
COPY pyproject.toml uv.lock ./
COPY src/ src/
COPY scripts/ scripts/
RUN ./scripts/gen_protos.sh \
    && uv sync --frozen --no-dev \
    && apt-get purge -y curl && apt-get autoremove -y

EXPOSE 8080

CMD ["uv", "run", "--frozen", "--no-dev", "hypercorn", "server.app:app", "--bind", "0.0.0.0:8080"]
