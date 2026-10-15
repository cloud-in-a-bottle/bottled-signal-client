# bottled-signal-client

A Signal messenger client as an OpenHost app. [signal-cli](https://github.com/AsamK/signal-cli) runs inside the
container as the **primary device** for a phone number; a web chat UI sits on top, and your phone can be linked to it as
a secondary device. Message history from a phone's on-device backup can be imported, and exported back as one.

## using it

1. **Register.** Open the app and choose *Register a phone number*: it asks for the number and whether to send the
   code by text or call, then for a captcha if Signal wants one (solve it at
   [signalcaptchas.org](https://signalcaptchas.org/registration/generate.html) and copy the *Open Signal* link), then the
   code, and the Signal PIN if the number has registration lock. Registering signs out whatever phone was the primary
   for that number.
2. **Set a profile name and PIN** in settings (⚙). The PIN turns on registration lock, which you want for a number
   whose primary lives on a server.
3. **Link your phone.** On the phone, choose to link to an existing account so it shows a QR code. In settings ›
   *Linked devices*, scan it with a camera, upload a screenshot, or paste the `sgnl://linkdevice?…` link. The phone gets
   contacts and groups, and everything from then on; it does **not** get past message history (signal-cli doesn't
   implement Signal's "link and sync" transfer).
4. **Import history.** On the phone, make an on-device backup (Settings › Chats › Backups), zip the `SignalBackups`
   folder, and in settings › *Import history from a backup* upload the zip with the 64-character recovery key. The zip
   goes up in 8MB chunks and an interrupted upload continues where it stopped. Importing is idempotent: messages are
   identified by author and sent timestamp, as Signal identifies them, so re-importing or overlapping with live
   messages doesn't duplicate anything.
5. **Move history to a phone (optional).** Settings › *Move history to a phone* builds an on-device backup of
   everything here, zipped, plus the account's recovery key. Unzip it onto a phone and restore it during Signal's setup.
   This moves the account back to the phone: the phone becomes the primary device and this server is signed out (the
   phone re-registers the number). Update notices and messages disappearing within a day are left out, as Signal's
   backup rules require; text styles and mentions go as plain text.

*Log out* (settings) disconnects the bridge from the account but leaves signal-cli registered, so nothing changes for
your contacts; *Use the account already on this server* picks it back up.

## how it works

The app is built on [bottled-bridge-core](https://github.com/cloud-in-a-bottle/bottled-bridge-core), shared with the
Messenger, WhatsApp and Instagram apps: a [mautrix bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2)
bridge whose "Matrix" side is a local message store, with the web UI and the
[messaging service](https://github.com/cloud-in-a-bottle/messaging-service-spec) on top; see its README. Unlike the
other apps, whose network connector is one of mautrix's, this one has its own, around signal-cli, because mautrix-signal
can only be a linked device. What's here:

- `internal/signalcli/`: runs `signal-cli daemon` (multi-account mode, so registering works before there's an account;
  `--receive-mode on-connection`, so messages stay queued on Signal's servers while the bridge isn't listening), and
  talks to its HTTP JSON-RPC API and event stream. Registering goes through the CLI, as the native 0.14.9 build can't
  read the JSON-RPC `register` parameters.
- `internal/connector/`: the bridgev2 network connector. Registration is its login flow (or *existing* takes over an
  account signal-cli already has); incoming envelopes become the bridge's remote events, and what the UI does becomes
  signal-cli calls. IDs follow mautrix-signal: people by ACI, chats by the other person's ACI or the group ID, messages
  by author and sent timestamp (an edited message takes its latest edit's).
- `internal/msgconv/`: Signal message content (text with mentions and styles, attachments, stickers, quotes, ...)
  as the bridge's message parts, shared by live messages and both importers, so a message is stored the same way
  however it arrives.
- `internal/backup/`: Signal's local backup format (the one shared by iOS/Android/Desktop since 8.30): key derivation,
  decryption and encryption, reading a zip, and writing a `SignalBackups` folder. The `.proto` schemas come from
  [libsignal](https://github.com/signalapp/libsignal) (AGPL-3.0) at a pinned commit, with their Go bindings in
  `backuppb/`; `just protos` updates them (bump the commit and checksums in `scripts/gen_protos.sh`).
- `internal/importer/`: imports a backup through the core's import API (`bridge.Service.ImportMessages`), which
  stores messages as if they had arrived live (so later reactions, replies and edits from Signal find them).
- `internal/exporter/`: writes the store (`store.Store.Export*`) as a backup a phone can restore. It needs the
  account's recovery key, profile key and group master keys, which signal-cli keeps but doesn't expose over JSON-RPC;
  `internal/signalcli/secrets.go` reads them from its storage.
- `internal/account/`: settings for the account itself (profile name, PIN, linked devices, decoding a device link's QR
  code from a picture).
- `internal/legacy/`: brings over the history of the Python version of this app, which kept its own tables in the
  `main` database: on startup they're renamed out of the store's way, the bridge logs in to the account signal-cli
  still has, and the history is imported (then the old tables are dropped). Attachment files were already where the
  store keeps them.
- `internal/static/`: the settings sections (account, import, export) the core's UI loads, and the icons.

Data: signal-cli's keys and state are in `$OPENHOST_APP_DATA_DIR/signal-cli` (losing them means registering again),
attachments in `$OPENHOST_APP_DATA_DIR/attachments`. The `main` database is the message history itself, the only copy
of it; the `bridge` database is the bridge's bookkeeping. Uploads in flight, exports and files being sent are in the
temp dir.

Message lifecycle rules follow Signal's: disappearing messages are deleted when their timer runs out (and already
expired ones are skipped on import), "delete for everyone" leaves a placeholder with the content removed, view-once
media is removed once viewed, and edits keep their history.

## limitations

- No message-history transfer to linked devices: a phone linked to this server starts with an empty history. (Signal's
  "link and sync" needs signal-cli to send a key while linking, which it doesn't support.) The only way to get the
  history onto a phone is *Move history to a phone*, which makes the phone the primary again.
- Legacy Android `.backup` files (30-digit passphrase) aren't imported; use an on-device backup instead.
- Cloud (Signal-hosted) backups can't be imported; they need Signal's servers to decrypt.
- Not handled: sending stickers, polls (shown as text), payments, stories, calls (no calling at all; call events from
  backups show as updates), sending mentions or text styles (the UI sends plain text).

## development

The app is Go; Python is only used for the container tests.

```bash
just setup   # go deps, the test harness, pre-commit hooks, and the playwright chromium browser
just run     # run locally on http://localhost:8080 with its data in ./dev-data (needs SIGNAL_CLI_BIN)
just test    # Go tests, then the container tests
just check   # gofmt, go vet, ruff
```

Go builds need `-tags sqlite_fts5` (FTS5 for search); the justfile sets `GOFLAGS` for that. Running outside a container
needs `SIGNAL_CLI_BIN`: a signal-cli native binary, from its [releases](https://github.com/AsamK/signal-cli/releases).

The Go tests run the real core (bridge, store) and this app's connector against a fake signal-cli daemon
(`internal/signaltest`): incoming envelopes, sending, receipts, edits, reactions, attachments, groups, view-once media,
backup imports (from backups the tests write with `internal/backup`), exports (read back, and imported again into a
fresh server), and the migration from the Python version's database. `tests/test_app.py` uses the OpenHost test
harness and needs podman.

To change the core alongside this app, check out bottled-bridge-core next to it and use a `go.work`
(`go work init . ../bottled-bridge-core`, not committed), push the core, then `just update-core <commit>`.

## license

AGPL-3.0, as it links mautrix (AGPL-3.0) and includes libsignal's backup protos (AGPL-3.0).
