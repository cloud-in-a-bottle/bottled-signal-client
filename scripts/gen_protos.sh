#!/bin/sh
# Download Signal's backup protos from libsignal at a pinned commit and generate Go bindings into
# internal/backup/backuppb/. Both are committed (libsignal is AGPL-3.0, as is this app); run this to update them, after
# bumping the commit and checksums. Needs protoc, and protoc-gen-go at the version of google.golang.org/protobuf in
# go.mod (go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12).
set -eu

LIBSIGNAL_COMMIT=4beb029d8a941f81e7d9c6d8af1ed25a677569a8
BASE_URL="https://raw.githubusercontent.com/signalapp/libsignal/${LIBSIGNAL_COMMIT}/rust/message-backup/src/proto"
PKG=github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb
OUT_DIR="$(dirname "$0")/../internal/backup/backuppb"

cd "$OUT_DIR"
curl -fsSL -o backup.proto "${BASE_URL}/backup.proto"
curl -fsSL -o LocalBackup.proto "${BASE_URL}/LocalBackup.proto"
sha256sum -c - <<SUMS
d63a708b3c7f2826f714174cd48b63649d5aad801f8550fa6cd78cabc19ab7f9  backup.proto
d593361c954be9a82d8bccd784cbfbb73a67b8f5c211b473cad4a9bb90ba0747  LocalBackup.proto
SUMS
protoc -I. --go_out=. --go_opt=paths=source_relative \
    --go_opt=Mbackup.proto=$PKG --go_opt=MLocalBackup.proto=$PKG backup.proto LocalBackup.proto
