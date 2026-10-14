#!/bin/sh
# Download Signal's backup protos from libsignal at a pinned commit and generate Python bindings into
# src/server/backup/proto/. libsignal is AGPL-3.0, so the protos are fetched at build time rather than committed.
set -eu

LIBSIGNAL_COMMIT=4beb029d8a941f81e7d9c6d8af1ed25a677569a8
GRPCIO_TOOLS_VERSION=1.84.0
BASE_URL="https://raw.githubusercontent.com/signalapp/libsignal/${LIBSIGNAL_COMMIT}/rust/message-backup/src/proto"
OUT_DIR="$(dirname "$0")/../src/server/backup/proto"

cd "$OUT_DIR"
curl -fsSL -o backup.proto "${BASE_URL}/backup.proto"
curl -fsSL -o LocalBackup.proto "${BASE_URL}/LocalBackup.proto"
sha256sum -c - <<SUMS
d63a708b3c7f2826f714174cd48b63649d5aad801f8550fa6cd78cabc19ab7f9  backup.proto
d593361c954be9a82d8bccd784cbfbb73a67b8f5c211b473cad4a9bb90ba0747  LocalBackup.proto
SUMS
uv run --no-project --with "grpcio-tools==${GRPCIO_TOOLS_VERSION}" \
    python -m grpc_tools.protoc -I. --python_out=. --pyi_out=. backup.proto LocalBackup.proto
