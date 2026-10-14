# bottled-signal-client

A Signal messenger client as an OpenHost app. [signal-cli](https://github.com/AsamK/signal-cli) runs inside the
container as the **primary device** for a phone number; a web chat UI sits on top, and your phone can be linked to it as
a secondary device. Message history from a phone's on-device backup can be imported, and exported back as one.

## using it

1. **Register.** Open the app; it asks for the number, a captcha token (solve it at
   [signalcaptchas.org](https://signalcaptchas.org/registration/generate.html), copy the *Open Signal* link) and then
   the SMS/voice code, plus your Signal PIN if the number has registration lock. Registering signs out whatever phone
   was the primary for that number.
2. **Set a profile name and PIN** in settings (⚙). The PIN turns on registration lock, which you want for a number
   whose primary lives on a server.
3. **Link your phone.** On the phone, choose to link to an existing account so it shows a QR code. In settings ›
   *Linked devices*, scan it with a camera, upload a screenshot, or paste the `sgnl://linkdevice?…` link. The phone gets
   contacts and groups, and everything from then on; it does **not** get past message history (signal-cli doesn't
   implement Signal's "link and sync" transfer).
4. **Import history.** On the phone, make an on-device backup (Settings › Chats › Backups), copy the `SignalBackups`
   folder to the computer you're browsing from, and in settings › *Import history* choose that folder and enter the
   64-character recovery key. Importing is idempotent: messages are keyed by (author, sent timestamp), so re-importing
   or overlapping with live messages doesn't duplicate anything.

5. **Move history to a phone (optional).** Settings › *Move history to a phone* builds an on-device backup of
   everything here, zipped, plus the account's recovery key. Unzip it onto a phone and restore it during Signal's setup.
   This moves the account back to the phone: the phone becomes the primary device and this server is signed out (the
   phone re-registers the number). Group update notices and messages disappearing within a day are left out, as
   Signal's backup rules require.

## how it works

- `src/server/signal/` — `daemon.py` supervises `signal-cli daemon` (multi-account mode, so `register`/`verify` work
  before an account exists; `--receive-mode on-connection`, so messages stay queued on Signal's servers while we're not
  listening). `rpc.py` and `sse.py` talk to its HTTP JSON-RPC and event stream. `receiver.py` turns incoming envelopes
  into rows; `service.py` is everything the UI asks signal-cli to do.
- `src/server/db/` — the message store (`schema.sql`). signal-cli keeps no history, so this is the only copy.
  Recipients are merged when Signal reveals that a phone number and an ACI are the same person. Attachments are stored
  content-addressed under `$OPENHOST_APP_DATA_DIR/attachments`.
- `src/server/backup/` — decryption (`crypto.py`) and import (`importer.py`) of Signal's local backup format, the one
  shared by iOS/Android/Desktop since 8.30. The `.proto` schemas come from
  [libsignal](https://github.com/signalapp/libsignal) (AGPL-3.0), so they aren't committed: `scripts/gen_protos.sh`
  downloads them at a pinned commit (checksum-verified) and generates the bindings. It runs in the Docker build and in
  `just setup`; bump the commit and checksums there to update.
- `src/server/backup/exporter.py` writes the reverse: our database as a local backup a phone can restore. It needs the
  account's recovery key (Account Entropy Pool) and profile key, and group master keys, which signal-cli keeps in its
  own storage but doesn't expose over JSON-RPC; `src/server/signal/account_file.py` reads them from there.
- `src/server/imports/` — backup uploads. The browser sends the folder in 8MB chunks, because the OpenHost router
  buffers each request body in memory; the import then runs in a thread, and the uploaded files are deleted afterwards.
- `src/server/static/` — the UI: plain JS modules, no build step. Live updates arrive over SSE (`/api/events`).

Data: signal-cli's keys and state are in `$OPENHOST_APP_DATA_DIR/signal-cli` (losing them means re-registering), the
database is the `main` sqlite, and uploads in flight live in the temp dir.

Message lifecycle rules follow Signal's: disappearing messages are deleted when their timer runs out (and already
expired ones are skipped on import), "delete for everyone" leaves a placeholder with the content removed, view-once
media is removed once viewed, and edits keep their history.

## limitations

- No message-history transfer to linked devices: a phone linked to this server starts with an empty history. (Signal's
  "link and sync" needs signal-cli to send a key while linking, which it doesn't support.) The only way to get the
  history onto a phone is *Move history to a phone*, which makes the phone the primary again.
- Legacy Android `.backup` files (30-digit passphrase) aren't imported; use an on-device backup instead.
- Cloud (Signal-hosted) backups can't be imported; they need Signal's servers to decrypt.
- Not yet handled: stickers sent live (shown as "[sticker]"), polls (shown as text), payments, stories, calls (no
  calling at all; call events from backups show as updates), mentions when sending.

## development

```bash
just setup   # install deps, pre-commit hooks, and the playwright chromium browser
just run     # run locally on http://localhost:8080 (needs the env vars below)
just test    # run the test suite
just check   # lint, format, typecheck
```

Running outside a container needs `OPENHOST_APP_DATA_DIR`, `OPENHOST_APP_TEMP_DIR`, `OPENHOST_SQLITE_MAIN` and
`SIGNAL_CLI_BIN` (a signal-cli native binary, eg from its
[releases](https://github.com/AsamK/signal-cli/releases)).

`tests/unit/` runs without containers: it drives the receiver with signal-cli-shaped envelopes, and builds real
encrypted backups (`tests/unit/backup_writer.py`) to test the importer end to end, and round-trips exports through
the importer. Set `SIGNAL_BACKUP_VALIDATOR` to libsignal's offline validator to also check exports against Signal's
rules (build it from libsignal with `cargo build --release -p libsignal-message-backup --features cli,test-util --bin
validator`). `tests/test_app.py` uses the OpenHost
test harness and needs podman.
