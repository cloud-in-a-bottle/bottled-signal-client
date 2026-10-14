import base64
import json
import os
import sqlite3
import time
import uuid
from collections.abc import Callable
from collections.abc import Iterator
from datetime import UTC
from datetime import datetime
from pathlib import Path

import attr
from google.protobuf.internal.containers import RepeatedCompositeFieldContainer
from loguru import logger

from server.backup import crypto
from server.backup import encrypt
from server.backup.proto import backup_pb2 as pb
from server.db.files import FileStore
from server.signal.account_file import AccountSecrets

# Signal rejects message bodies over 128 KiB in a backup.
_MAX_BODY_BYTES = 128 * 1024
# A local (and remote) backup may not contain disappearing messages with less than a day left; the phone would only
# have to delete them straight away.
_MIN_EXPIRY_LEFT_MS = 24 * 3600 * 1000
_APP_VERSION = "bottled-signal-client"

# (message, progress 0..1)
ExportProgressFn = Callable[[str, float], None]


@attr.s(auto_attribs=True, frozen=True)
class ExportResult:
    backup_dir: Path
    messages_exported: int
    messages_skipped: int
    attachments_exported: int


def _uuid_bytes(value: str) -> bytes:
    return uuid.UUID(value).bytes


def _split_name(name: str | None) -> tuple[str, str]:
    if not name:
        return "", ""
    given, _, family = name.partition(" ")
    return given, family


class _Exporter:
    """Turns the message database into Signal backup frames. Recipient and chat ids in the backup are our own row ids,
    which are non-zero and unique as the format requires."""

    def __init__(
        self,
        conn: sqlite3.Connection,
        files: FileStore,
        secrets: AccountSecrets,
        files_dir: Path,
        now_ms: int,
        progress: ExportProgressFn,
    ) -> None:
        self._conn = conn
        self._files = files
        self._secrets = secrets
        self._files_dir = files_dir
        self._now = now_ms
        self._progress = progress
        self._self_id = int(conn.execute("SELECT id FROM recipients WHERE kind = 'self'").fetchone()["id"])
        self._emitted: set[int] = set()
        self._chat_ids: dict[int, int] = {}  # conversation id -> chat id
        self._chat_kind: dict[int, str] = {}  # conversation id -> recipient kind
        self.exported = 0
        self.skipped = 0
        self.attachments = 0

    def frames(self) -> Iterator[bytes]:
        info = pb.BackupInfo(
            version=1,
            backupTimeMs=self._now,
            mediaRootBackupKey=self._secrets.media_root_backup_key or os.urandom(32),
            currentAppVersion=_APP_VERSION,
            firstAppVersion=_APP_VERSION,
        )
        yield info.SerializeToString()
        yield pb.Frame(account=self._account()).SerializeToString()
        for recipient in self._recipients():
            self._emitted.add(recipient.id)
            yield pb.Frame(recipient=recipient).SerializeToString()
        for chat in self._chats():
            yield pb.Frame(chat=chat).SerializeToString()
        total = max(1, int(self._conn.execute("SELECT count(*) FROM messages").fetchone()[0]))
        for i, item in enumerate(self._chat_items()):
            if item is not None:
                yield pb.Frame(chatItem=item).SerializeToString()
            if i % 200 == 0:
                self._progress("Exporting messages", min(1.0, (self.exported + self.skipped) / total))

    # --- account, recipients, chats -------------------------------------------------------------------------

    def _account(self) -> pb.AccountData:
        given, family = _split_name(
            self._conn.execute("SELECT profile_name FROM recipients WHERE id = ?", (self._self_id,)).fetchone()[0]
        )
        account = pb.AccountData(profileKey=self._secrets.profile_key, givenName=given, familyName=family)
        s = account.accountSettings
        s.readReceipts = True
        s.sealedSenderIndicators = False
        s.typingIndicators = True
        s.linkPreviews = True
        s.phoneNumberSharingMode = pb.AccountData.NOBODY
        s.defaultSentMediaQuality = pb.AccountData.STANDARD
        s.appTheme = pb.AccountData.SYSTEM
        s.callsUseLessDataSetting = pb.AccountData.NEVER
        return account

    def _recipients(self) -> Iterator[pb.Recipient]:
        yield pb.Recipient(id=self._self_id, self=pb.Self())
        for r in self._conn.execute(
            "SELECT * FROM recipients WHERE kind = 'contact' AND (aci IS NOT NULL OR e164 IS NOT NULL) ORDER BY id"
        ):
            recipient = pb.Recipient(id=r["id"])
            c = recipient.contact
            if r["aci"]:
                c.aci = _uuid_bytes(r["aci"])
            if r["pni"]:
                c.pni = _uuid_bytes(r["pni"])
            if r["e164"]:
                c.e164 = int(r["e164"].lstrip("+"))
            c.registered.SetInParent()
            c.profileGivenName, c.profileFamilyName = _split_name(r["profile_name"])
            c.systemGivenName, c.systemFamilyName = _split_name(r["contact_name"])
            yield recipient
        for r in self._conn.execute(
            "SELECT * FROM recipients WHERE kind = 'group' AND group_master_key IS NOT NULL ORDER BY id"
        ):
            recipient = pb.Recipient(id=r["id"])
            g = recipient.group
            g.masterKey = base64.b64decode(r["group_master_key"])
            g.whitelisted = True
            if r["group_title"]:
                g.snapshot.title.title = r["group_title"]
            for m in self._conn.execute(
                "SELECT r.aci, gm.role FROM group_members gm JOIN recipients r ON r.id = gm.member_recipient_id "
                "WHERE gm.group_recipient_id = ? AND r.aci IS NOT NULL",
                (r["id"],),
            ):
                role = pb.Group.Member.ADMINISTRATOR if m["role"] == "admin" else pb.Group.Member.DEFAULT
                g.snapshot.members.add(userId=_uuid_bytes(m["aci"]), role=role)
            yield recipient

    def _chats(self) -> Iterator[pb.Chat]:
        pinned_order = 0
        for c in self._conn.execute(
            "SELECT c.*, r.kind FROM conversations c JOIN recipients r ON r.id = c.recipient_id ORDER BY c.id"
        ):
            if c["recipient_id"] not in self._emitted:
                continue
            chat = pb.Chat(id=c["id"], recipientId=c["recipient_id"], archived=bool(c["archived"]))
            if c["pinned"]:
                pinned_order += 1
                chat.pinnedOrder = pinned_order
            if c["expire_timer_s"]:
                chat.expirationTimerMs = c["expire_timer_s"] * 1000
                chat.expireTimerVersion = 1
            if c["muted_until"] > self._now:
                chat.muteUntilMs = c["muted_until"]
            self._chat_ids[c["id"]] = c["id"]
            self._chat_kind[c["id"]] = c["kind"]
            yield chat

    # --- chat items -----------------------------------------------------------------------------------------

    def _chat_items(self) -> Iterator[pb.ChatItem | None]:
        conversations = self._conn.execute("SELECT id, last_read_at FROM conversations").fetchall()
        last_read = {c["id"]: c["last_read_at"] for c in conversations}
        for m in self._conn.execute("SELECT * FROM messages ORDER BY sent_at, id"):
            item = self._chat_item(m, last_read.get(m["conversation_id"], 0))
            if item is None:
                self.skipped += 1
            else:
                self.exported += 1
            yield item

    def _chat_item(self, m: sqlite3.Row, last_read_at: int) -> pb.ChatItem | None:
        chat_id = self._chat_ids.get(m["conversation_id"])
        author = m["author_id"]
        if chat_id is None or author not in self._emitted:
            return None
        if m["expires_at"] is not None and m["expires_at"] - self._now < _MIN_EXPIRY_LEFT_MS:
            return None

        item = pb.ChatItem(chatId=chat_id, authorId=author, dateSent=m["edited_at"] or m["sent_at"])
        outgoing = author == self._self_id
        read = m["sent_at"] <= last_read_at
        if m["kind"] == "update":
            if not self._fill_update(item, m):
                return None
            item.directionless.SetInParent()
        else:
            if not self._fill_content(item, m):
                return None
            self._fill_direction(item, m, outgoing, read)
            for revision in self._revisions(m, outgoing, read):
                item.revisions.append(revision)

        if m["expire_timer_s"]:
            item.expiresInMs = m["expire_timer_s"] * 1000
            started = m["expire_started_at"]
            if started is None and (outgoing or read):
                # The format requires a start once a message is read or sent.
                started = m["received_at"]
            if started is not None:
                item.expireStartDate = started
        return item

    def _fill_direction(self, item: pb.ChatItem, m: sqlite3.Row, outgoing: bool, read: bool) -> None:
        if not outgoing:
            item.incoming.dateReceived = m["received_at"]
            if m["server_received_at"]:
                item.incoming.dateServerSent = m["server_received_at"]
            item.incoming.read = read
            return
        item.outgoing.dateReceived = m["received_at"]
        for r in self._conn.execute("SELECT * FROM receipts WHERE message_id = ?", (m["id"],)):
            if r["recipient_id"] not in self._emitted or r["recipient_id"] == self._self_id:
                continue
            status = item.outgoing.sendStatus.add(recipientId=r["recipient_id"], timestamp=r["at"])
            getattr(status, r["status"]).SetInParent()

    def _revisions(self, m: sqlite3.Row, outgoing: bool, read: bool) -> Iterator[pb.ChatItem]:
        for e in self._conn.execute("SELECT * FROM message_edits WHERE message_id = ? ORDER BY revision", (m["id"],)):
            if not e["body"]:
                continue
            revision = pb.ChatItem(chatId=self._chat_ids[m["conversation_id"]], authorId=m["author_id"])
            revision.dateSent = e["edited_at"]
            self._set_text(revision.standardMessage, e["body"], e["body_ranges"])
            if outgoing:
                revision.outgoing.dateReceived = e["edited_at"]
            else:
                revision.incoming.dateReceived = e["edited_at"]
                revision.incoming.read = read
            yield revision

    def _set_text(self, message: pb.StandardMessage, body: str, body_ranges: str | None) -> None:
        if len(body.encode()) > _MAX_BODY_BYTES:
            body = body.encode()[:_MAX_BODY_BYTES].decode(errors="ignore")
            body_ranges = None
        message.text.body = body
        for r in json.loads(body_ranges) if body_ranges else []:
            if r["start"] + r["length"] > len(body.encode("utf-16-le")) // 2:
                continue
            if "mentionAci" in r:
                message.text.bodyRanges.add(
                    start=r["start"], length=r["length"], mentionAci=_uuid_bytes(r["mentionAci"])
                )
            elif r.get("style") in pb.BodyRange.Style.keys():
                message.text.bodyRanges.add(
                    start=r["start"], length=r["length"], style=pb.BodyRange.Style.Value(r["style"])
                )

    def _fill_content(self, item: pb.ChatItem, m: sqlite3.Row) -> bool:
        if m["deleted_at"] is not None:
            item.remoteDeletedMessage.SetInParent()
            return True
        attachments = self._conn.execute(
            "SELECT * FROM attachments WHERE message_id = ? ORDER BY ordinal", (m["id"],)
        ).fetchall()
        if m["view_once"]:
            if not attachments:
                return False
            item.viewOnceMessage.attachment.pointer.CopyFrom(self._pointer(attachments[0]))
            self._add_reactions(item.viewOnceMessage.reactions, m["id"])
            return True
        if not m["body"] and not attachments:
            return False
        message = item.standardMessage
        if m["body"]:
            self._set_text(message, m["body"], m["body_ranges"])
        for a in attachments:
            ma = message.attachments.add(pointer=self._pointer(a))
            if a["is_voice_note"]:
                ma.flag = pb.MessageAttachment.VOICE_MESSAGE
        if m["quote_sent_at"] is not None and m["quote_author_id"] in self._emitted:
            message.quote.targetSentTimestamp = m["quote_sent_at"]
            message.quote.authorId = m["quote_author_id"]
            message.quote.type = pb.Quote.NORMAL
            if m["quote_text"]:
                message.quote.text.body = m["quote_text"]
        self._add_reactions(message.reactions, m["id"])
        return True

    def _add_reactions(self, reactions: RepeatedCompositeFieldContainer[pb.Reaction], message_id: int) -> None:
        for order, r in enumerate(
            self._conn.execute("SELECT * FROM reactions WHERE message_id = ? ORDER BY sent_at", (message_id,))
        ):
            if r["reactor_id"] in self._emitted:
                reactions.add(
                    emoji=r["emoji"], authorId=r["reactor_id"], sentTimestamp=r["sent_at"], sortOrder=order + 1
                )

    def _pointer(self, a: sqlite3.Row) -> pb.FilePointer:
        pointer = pb.FilePointer()
        if a["content_type"]:
            pointer.contentType = a["content_type"]
        if a["filename"]:
            pointer.fileName = a["filename"]
        if a["width"]:
            pointer.width = a["width"]
        if a["height"]:
            pointer.height = a["height"]
        if a["caption"]:
            pointer.caption = a["caption"]
        if a["blurhash"]:
            pointer.blurHash = a["blurhash"]
        path = self._files.path_for(a["file_sha256"]) if a["status"] == "present" and a["file_sha256"] else None
        if path is None or not path.exists():
            # An empty locator marks the attachment as unavailable.
            pointer.locatorInfo.SetInParent()
            return pointer
        written = encrypt.write_attachment(self._files_dir, path)
        loc = pointer.locatorInfo
        # The transit key is required to be present but is only used for CDN downloads, which local restores don't do.
        loc.key = os.urandom(64)
        loc.plaintextHash = written.plaintext_hash
        loc.size = written.size
        loc.localKey = written.local_key
        self.attachments += 1
        return pointer

    def _fill_update(self, item: pb.ChatItem, m: sqlite3.Row) -> bool:
        payload = json.loads(m["update_payload"]) if m["update_payload"] else {}
        u = item.updateMessage
        kind = payload.get("type")
        if kind == "expirationTimer":
            u.expirationTimerChange.expiresInMs = int(payload.get("seconds", 0)) * 1000
        elif kind == "call" and self._chat_kind.get(m["conversation_id"]) == "contact":
            c = u.individualCall
            c.type = pb.IndividualCall.VIDEO_CALL if payload.get("video") else pb.IndividualCall.AUDIO_CALL
            c.direction = (
                pb.IndividualCall.OUTGOING if payload.get("direction") == "outgoing" else pb.IndividualCall.INCOMING
            )
            c.state = pb.IndividualCall.MISSED if payload.get("missed") else pb.IndividualCall.ACCEPTED
            c.startedCallTimestamp = m["sent_at"]
            c.read = True
        elif kind in ("joinedSignal", "safetyNumberChange", "chatSessionRefresh"):
            simple = {
                "joinedSignal": pb.SimpleChatUpdate.JOINED_SIGNAL,
                "safetyNumberChange": pb.SimpleChatUpdate.IDENTITY_UPDATE,
                "chatSessionRefresh": pb.SimpleChatUpdate.CHAT_SESSION_REFRESH,
            }[kind]
            u.simpleUpdate.type = simple
        else:
            # Group changes and other updates we only keep a summary of can't be reconstructed faithfully.
            return False
        return True


def export_local_backup(
    conn: sqlite3.Connection,
    files: FileStore,
    secrets: AccountSecrets,
    out_root: Path,
    progress: ExportProgressFn,
) -> ExportResult:
    """Write a `SignalBackups` folder (one snapshot plus `files/`) under `out_root` that a phone can restore from with
    the account's recovery key, in the same layout the Signal apps produce."""
    now = int(time.time() * 1000)
    backups = out_root / "SignalBackups"
    stamp = datetime.fromtimestamp(now / 1000, UTC).strftime("%Y-%m-%d-%H-%M-%S")
    snapshot = backups / f"signal-backup-{stamp}"
    snapshot.mkdir(parents=True)
    files_dir = backups / "files"
    files_dir.mkdir()

    bk = crypto.backup_key(secrets.recovery_key)
    # iOS checks that the backup id matches the one derived from the recovery key and the account's ACI.
    backup_id = crypto.backup_id_from_aci(bk, _uuid_bytes(secrets.aci))
    hmac_key, aes_key = crypto.message_backup_keys(bk, backup_id)

    exporter = _Exporter(conn, files, secrets, files_dir, now, progress)
    encrypt.write_main(snapshot / "main", hmac_key, aes_key, exporter.frames())
    encrypt.write_metadata(snapshot / "metadata", bk, backup_id)
    logger.info(
        "exported {} messages ({} skipped), {} attachments", exporter.exported, exporter.skipped, exporter.attachments
    )
    return ExportResult(
        backup_dir=backups,
        messages_exported=exporter.exported,
        messages_skipped=exporter.skipped,
        attachments_exported=exporter.attachments,
    )
