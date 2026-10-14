import base64
import json
import sqlite3
import time
import uuid
from collections.abc import Callable
from collections.abc import Iterable
from pathlib import Path

import attr
from loguru import logger

from server.backup import crypto
from server.backup.proto import LocalBackup_pb2
from server.backup.proto import backup_pb2 as pb
from server.db import messages as dbm
from server.db import recipients as dbr
from server.db.files import FileStore

_BATCH = 500

# (message, progress 0..1, messages imported, messages skipped, attachments imported)
ProgressFn = Callable[[str, float, int, int, int], None]


@attr.s(auto_attribs=True, frozen=True)
class ImportResult:
    messages_imported: int
    messages_skipped: int
    attachments_imported: int


@attr.s(auto_attribs=True, frozen=True)
class LocalBackup:
    snapshot_dir: Path
    files_dir: Path | None


def _uuid_str(raw: bytes) -> str:
    return str(uuid.UUID(bytes=raw))


def _now_ms() -> int:
    return int(time.time() * 1000)


def _join_name(*parts: str) -> str | None:
    name = " ".join(p for p in parts if p)
    return name or None


class _Importer:
    def __init__(self, conn: sqlite3.Connection, files: FileStore, files_dir: Path | None, progress: ProgressFn):
        self._conn = conn
        self._files = files
        self._files_dir = files_dir
        self._progress = progress
        self._recipients: dict[int, int] = {}  # backup recipient id -> our recipient id
        self._chats: dict[int, int] = {}  # backup chat id -> our conversation's recipient id
        self._read_up_to: dict[int, int] = {}  # our conversation id -> newest read incoming sent_at
        self._self_id = dbr.self_recipient_id(conn)
        self._fraction = 0.0
        self.imported = 0
        self.skipped = 0
        self.attachments = 0

    def set_fraction(self, fraction: float) -> None:
        self._fraction = fraction

    def report(self, message: str) -> None:
        self._progress(message, self._fraction, self.imported, self.skipped, self.attachments)

    def frame(self, frame: pb.Frame) -> None:
        which = frame.WhichOneof("item")
        if which == "recipient":
            self._recipient(frame.recipient)
        elif which == "chat":
            self._chat(frame.chat)
        elif which == "chatItem":
            self._chat_item(frame.chatItem)

    def finish(self) -> None:
        for conversation_id, sent_at in self._read_up_to.items():
            self._conn.execute(
                "UPDATE conversations SET last_read_at = max(last_read_at, ?) WHERE id = ?", (sent_at, conversation_id)
            )

    # --- recipients and chats -------------------------------------------------------------------------------

    def _recipient(self, r: pb.Recipient) -> None:
        which = r.WhichOneof("destination")
        if which == "self":
            self._recipients[r.id] = self._self_id
        elif which == "contact":
            c = r.contact
            ids = dbr.ContactIds(
                aci=_uuid_str(c.aci) if c.HasField("aci") and len(c.aci) == 16 else None,
                pni=_uuid_str(c.pni) if c.HasField("pni") and len(c.pni) == 16 else None,
                e164=f"+{c.e164}" if c.HasField("e164") and c.e164 else None,
            )
            if ids.is_empty():
                return
            recipient_id = dbr.resolve_contact(self._conn, ids)
            contact_name = _join_name(c.nickname.given, c.nickname.family) or _join_name(
                c.systemGivenName, c.systemFamilyName
            )
            profile_name = _join_name(c.profileGivenName, c.profileFamilyName)
            # Don't overwrite names already learned live, which are newer than the backup.
            row = self._conn.execute(
                "SELECT profile_name, contact_name FROM recipients WHERE id = ?", (recipient_id,)
            ).fetchone()
            dbr.update_contact_names(
                self._conn,
                recipient_id,
                None if row["profile_name"] else profile_name,
                None if row["contact_name"] else contact_name,
            )
            self._recipients[r.id] = recipient_id
        elif which == "group":
            g = r.group
            group_id = base64.b64encode(crypto.group_id_from_master_key(g.masterKey)).decode()
            recipient_id = dbr.resolve_group(self._conn, group_id)
            title = g.snapshot.title.title if g.snapshot.title.WhichOneof("content") == "title" else None
            existing = self._conn.execute(
                "SELECT group_title FROM recipients WHERE id = ?", (recipient_id,)
            ).fetchone()
            dbr.update_group(
                self._conn,
                recipient_id,
                None if existing["group_title"] else title,
                base64.b64encode(g.masterKey).decode(),
            )
            has_members = self._conn.execute(
                "SELECT 1 FROM group_members WHERE group_recipient_id = ?", (recipient_id,)
            ).fetchone()
            if not has_members:
                members = []
                for m in g.snapshot.members:
                    if len(m.userId) != 16:
                        continue
                    member_id = dbr.resolve_contact(self._conn, dbr.ContactIds(aci=_uuid_str(m.userId)))
                    members.append((member_id, "admin" if m.role == pb.Group.Member.ADMINISTRATOR else "member"))
                dbr.set_group_members(self._conn, recipient_id, members)
            self._recipients[r.id] = recipient_id
        # Distribution lists (stories), release notes and call links have no place here.

    def _chat(self, chat: pb.Chat) -> None:
        recipient_id = self._recipients.get(chat.recipientId)
        if recipient_id is None:
            return
        self._chats[chat.id] = recipient_id
        conversation_id = dbm.conversation_for(self._conn, recipient_id)
        timer_s = chat.expirationTimerMs // 1000 if chat.HasField("expirationTimerMs") else 0
        mute_until = min(chat.muteUntilMs, 2**62) if chat.HasField("muteUntilMs") else 0
        self._conn.execute(
            """
            UPDATE conversations SET archived = ?, pinned = ?, muted_until = max(muted_until, ?),
                expire_timer_s = CASE WHEN last_message_at = 0 THEN ? ELSE expire_timer_s END
            WHERE id = ?
            """,
            (int(chat.archived), int(chat.HasField("pinnedOrder")), mute_until, timer_s, conversation_id),
        )

    # --- messages -------------------------------------------------------------------------------------------

    def _chat_item(self, item: pb.ChatItem) -> None:
        conversation_recipient = self._chats.get(item.chatId)
        author_id = self._recipients.get(item.authorId)
        if conversation_recipient is None or author_id is None:
            return

        expire_timer_s = item.expiresInMs // 1000 if item.HasField("expiresInMs") else 0
        expire_started_at = item.expireStartDate if item.HasField("expireStartDate") else None
        if expire_timer_s and expire_started_at and expire_started_at + expire_timer_s * 1000 <= _now_ms():
            return  # already disappeared; the sender asked for it to be gone

        built = self._content(item)
        if built is None:
            return
        direction = item.WhichOneof("directionalDetails")
        received_at = item.dateSent
        server_received_at = None
        if direction == "incoming":
            received_at = item.incoming.dateReceived or item.dateSent
            if item.incoming.HasField("dateServerSent"):
                server_received_at = item.incoming.dateServerSent
        elif direction == "outgoing":
            received_at = item.outgoing.dateReceived or item.dateSent

        msg = attr.evolve(
            built,
            conversation_recipient_id=conversation_recipient,
            author_id=author_id,
            # For an edited message the item carries the newest edit's timestamp; the message is identified (and
            # targeted by reactions, quotes and later edits) by the original's, which is the oldest revision.
            sent_at=item.revisions[0].dateSent if item.revisions else item.dateSent,
            received_at=received_at,
            server_received_at=server_received_at,
            expire_timer_s=expire_timer_s,
            expire_started_at=expire_started_at,
        )
        stored = dbm.insert_message(self._conn, msg)
        if not stored.created:
            self.skipped += 1
            # The attachment files were decrypted before we knew this was a duplicate.
            shas = [a.file_sha256 for a in msg.attachments if a.file_sha256 is not None]
            self._files.delete(dbm.unreferenced(self._conn, shas))
            return
        self.imported += 1
        self.attachments += sum(1 for a in msg.attachments if a.status == "present")

        if self._is_deleted(item):
            self._conn.execute("UPDATE messages SET deleted_at = ? WHERE id = ?", (item.dateSent, stored.id))
        for i, revision in enumerate(item.revisions):
            body, ranges = self._text_of(revision)
            self._conn.execute(
                "INSERT INTO message_edits (message_id, revision, body, body_ranges, edited_at) VALUES (?, ?, ?, ?, ?)",
                (stored.id, i, body, ranges, revision.dateSent),
            )
        if item.revisions:
            self._conn.execute("UPDATE messages SET edited_at = ? WHERE id = ?", (item.dateSent, stored.id))
        for reaction in self._reactions_of(item):
            reactor = self._recipients.get(reaction.authorId)
            if reactor is not None and reaction.emoji:
                dbm.set_reaction(self._conn, stored.id, reactor, reaction.emoji, reaction.sentTimestamp)
        if direction == "outgoing":
            for status in item.outgoing.sendStatus:
                self._send_status(stored.id, status)
        elif direction == "incoming" and item.incoming.read:
            current = self._read_up_to.get(stored.conversation_id, 0)
            self._read_up_to[stored.conversation_id] = max(current, item.dateSent)

        if (self.imported + self.skipped) % _BATCH == 0:
            self._conn.execute("COMMIT")
            self._conn.execute("BEGIN IMMEDIATE")
            self.report("Importing messages")

    def _send_status(self, message_id: int, status: pb.SendStatus) -> None:
        recipient = self._recipients.get(status.recipientId)
        which = status.WhichOneof("deliveryStatus")
        mapped = {"sent": "sent", "delivered": "delivered", "read": "read", "viewed": "viewed"}.get(which or "")
        if recipient is not None and mapped is not None:
            dbm.set_receipt(self._conn, message_id, recipient, mapped, status.timestamp)

    @staticmethod
    def _is_deleted(item: pb.ChatItem) -> bool:
        return item.WhichOneof("item") in ("remoteDeletedMessage", "adminDeletedMessage")

    def _reactions_of(self, item: pb.ChatItem) -> list[pb.Reaction]:
        which = item.WhichOneof("item")
        if which in (
            "standardMessage",
            "contactMessage",
            "stickerMessage",
            "viewOnceMessage",
            "poll",
            "directStoryReplyMessage",
        ):
            return list(getattr(item, which).reactions)
        return []

    def _text_of(self, item: pb.ChatItem) -> tuple[str | None, str | None]:
        if item.WhichOneof("item") != "standardMessage":
            return None, None
        return self._text(item.standardMessage)

    def _text(self, m: pb.StandardMessage) -> tuple[str | None, str | None]:
        body = m.text.body if m.HasField("text") else None
        if m.HasField("longText"):
            long_body = self._read_long_text(m.longText)
            if long_body is not None:
                body = long_body
        ranges = self._ranges(m.text.bodyRanges) if m.HasField("text") else None
        return body or None, ranges

    @staticmethod
    def _ranges(ranges: Iterable[pb.BodyRange]) -> str | None:
        out: list[dict[str, str | int]] = []
        for r in ranges:
            which = r.WhichOneof("associatedValue")
            if which == "mentionAci" and len(r.mentionAci) == 16:
                out.append({"start": r.start, "length": r.length, "mentionAci": _uuid_str(r.mentionAci)})
            elif which == "style" and r.style != pb.BodyRange.NONE:
                out.append({"start": r.start, "length": r.length, "style": pb.BodyRange.Style.Name(r.style)})
        return json.dumps(out) if out else None

    def _content(self, item: pb.ChatItem) -> dbm.NewMessage | None:
        """The message body/attachments/update payload; identity fields are filled in by the caller."""
        which = item.WhichOneof("item")
        base = dbm.NewMessage(
            conversation_recipient_id=0,
            author_id=0,
            sent_at=0,
            received_at=0,
            server_received_at=None,
            kind="text",
            body=None,
            body_ranges=None,
            source="backup",
        )
        if which == "standardMessage":
            m = item.standardMessage
            body, ranges = self._text(m)
            quote_author = quote_sent_at = quote_text = None
            if m.HasField("quote") and m.quote.HasField("targetSentTimestamp"):
                quote_author = self._recipients.get(m.quote.authorId)
                quote_sent_at = m.quote.targetSentTimestamp
                quote_text = m.quote.text.body if m.quote.HasField("text") else None
            attachments = tuple(
                self._attachment(a.pointer, a.flag == pb.MessageAttachment.VOICE_MESSAGE) for a in m.attachments
            )
            if body is None and not attachments:
                return None
            return attr.evolve(
                base,
                body=body,
                body_ranges=ranges,
                quote_author_id=quote_author,
                quote_sent_at=quote_sent_at if quote_author is not None else None,
                quote_text=quote_text,
                attachments=attachments,
            )
        if which in ("remoteDeletedMessage", "adminDeletedMessage"):
            return base
        if which == "viewOnceMessage":
            v = item.viewOnceMessage
            attachments = (self._attachment(v.attachment.pointer, False),) if v.HasField("attachment") else ()
            return attr.evolve(base, view_once=True, attachments=attachments)
        if which == "stickerMessage":
            s = item.stickerMessage.sticker
            attachments = (self._attachment(s.data, False),) if s.HasField("data") else ()
            return attr.evolve(base, body=None if attachments else (s.emoji or "[sticker]"), attachments=attachments)
        if which == "contactMessage":
            c = item.contactMessage.contact
            name = _join_name(c.name.givenName, c.name.familyName) if c.HasField("name") else None
            numbers = ", ".join(n.value for n in c.number if n.value)
            return attr.evolve(base, body=f"[shared contact] {name or ''} {numbers}".strip())
        if which == "poll":
            p = item.poll
            options = "\n".join(f"• {o.option}" for o in p.options)
            return attr.evolve(base, body=f"[poll] {p.question}\n{options}")
        if which == "directStoryReplyMessage":
            d = item.directStoryReplyMessage
            if d.WhichOneof("reply") == "emoji":
                return attr.evolve(base, body=f"[reacted to a story] {d.emoji}")
            return attr.evolve(base, body=d.textReply.text.body or None)
        if which == "updateMessage":
            payload = self._update_payload(item.updateMessage)
            if payload is None:
                return None
            return attr.evolve(base, kind="update", update_payload=json.dumps(payload))
        # Payment notifications, gift badges and unknown future types are skipped.
        return None

    def _update_payload(self, u: pb.ChatUpdateMessage) -> dict[str, str | int | bool] | None:
        which = u.WhichOneof("update")
        if which == "simpleUpdate":
            t = pb.SimpleChatUpdate.Type.Name(u.simpleUpdate.type)
            mapping = {
                "JOINED_SIGNAL": "joinedSignal",
                "IDENTITY_UPDATE": "safetyNumberChange",
                "CHAT_SESSION_REFRESH": "chatSessionRefresh",
            }
            if t in mapping:
                return {"type": mapping[t]}
            if t == "UNKNOWN":
                return None
            return {"type": "simple", "text": t.replace("_", " ").capitalize()}
        if which == "groupChange":
            return {"type": "groupUpdate"}
        if which == "expirationTimerChange":
            return {"type": "expirationTimer", "seconds": u.expirationTimerChange.expiresInMs // 1000}
        if which == "profileChange":
            return {"type": "profileChange"}
        if which == "individualCall":
            c = u.individualCall
            return {
                "type": "call",
                "direction": "outgoing" if c.direction == pb.IndividualCall.OUTGOING else "incoming",
                "video": c.type == pb.IndividualCall.VIDEO_CALL,
                "missed": c.state in (pb.IndividualCall.MISSED, pb.IndividualCall.MISSED_NOTIFICATION_PROFILE),
            }
        if which == "groupCall":
            return {"type": "groupCall"}
        return None

    # --- attachments ----------------------------------------------------------------------------------------

    def _decrypt_local_file(self, pointer: pb.FilePointer) -> str | None:
        """Decrypt a local-backup attachment into the file store, returning its sha256, or None if not present."""
        loc = pointer.locatorInfo
        if (
            self._files_dir is None
            or not pointer.HasField("locatorInfo")
            or not loc.HasField("localKey")
            or loc.WhichOneof("integrityCheck") != "plaintextHash"
        ):
            return None
        name = crypto.local_media_name(loc.plaintextHash, loc.localKey)
        path = self._files_dir / name[:2] / name
        if not path.exists():
            upper = self._files_dir / name[:2].upper() / name.upper()
            if not upper.exists():
                return None
            path = upper
        try:
            return self._files.put_stream(crypto.decrypt_attachment(path, loc.localKey, loc.size, loc.plaintextHash))
        except crypto.BackupDecryptError as e:
            logger.warning("skipping attachment {}: {}", name, e)
            return None

    def _attachment(self, pointer: pb.FilePointer, is_voice_note: bool) -> dbm.NewAttachment:
        sha = self._decrypt_local_file(pointer)
        return dbm.NewAttachment(
            content_type=pointer.contentType if pointer.HasField("contentType") else None,
            filename=pointer.fileName if pointer.HasField("fileName") else None,
            size=pointer.locatorInfo.size or None,
            width=pointer.width if pointer.HasField("width") else None,
            height=pointer.height if pointer.HasField("height") else None,
            duration_ms=None,
            caption=pointer.caption if pointer.HasField("caption") else None,
            blurhash=pointer.blurHash if pointer.HasField("blurHash") else None,
            is_voice_note=is_voice_note,
            file_sha256=sha,
            status="present" if sha is not None else "missing",
        )

    def _read_long_text(self, pointer: pb.FilePointer) -> str | None:
        sha = self._decrypt_local_file(pointer)
        if sha is None:
            return None
        path = self._files.path_for(sha)
        text = path.read_text(errors="replace")
        # Long text is folded into the body, so don't keep the separate file unless something else references it.
        if self._conn.execute("SELECT 1 FROM attachments WHERE file_sha256 = ?", (sha,)).fetchone() is None:
            path.unlink(missing_ok=True)
        return text


def find_local_backup(root: Path) -> LocalBackup:
    """Locate the newest complete snapshot (a dir holding `main` + `metadata`) and the shared `files/` dir in an
    uploaded tree. Accepts the SignalBackups folder, a single snapshot folder, or a snapshot plus files."""
    snapshots = sorted(
        (
            p.parent
            for p in root.rglob("metadata")
            if (p.parent / "main").is_file() and not p.parent.name.endswith("-tmp")
        ),
        key=lambda p: p.name,
    )
    if not snapshots:
        if any(p.suffix == ".backup" for p in root.rglob("*.backup")):
            raise crypto.BackupDecryptError(
                "this looks like a legacy Android .backup file, which isn't supported yet; export an on-device backup "
                "(Settings › Chats › Backups) instead"
            )
        raise crypto.BackupDecryptError(
            "couldn't find a backup (a folder containing 'main' and 'metadata') in the upload"
        )
    snapshot = snapshots[-1]
    files_dir = None
    for candidate in (snapshot.parent / "files", snapshot / "files", root / "files"):
        if candidate.is_dir():
            files_dir = candidate
            break
    return LocalBackup(snapshot_dir=snapshot, files_dir=files_dir)


def import_local_backup(
    backup: LocalBackup,
    recovery_key: str,
    conn: sqlite3.Connection,
    files: FileStore,
    progress: ProgressFn,
) -> ImportResult:
    metadata = LocalBackup_pb2.Metadata()
    metadata.ParseFromString((backup.snapshot_dir / "metadata").read_bytes())
    if metadata.version != 1:
        raise crypto.BackupDecryptError(f"unsupported backup metadata version {metadata.version}")
    bk = crypto.backup_key(crypto.normalize_recovery_key(recovery_key))
    backup_id = crypto.backup_id_from_metadata(bk, metadata.backupId.iv, metadata.backupId.encryptedId)
    hmac_key, aes_key = crypto.message_backup_keys(bk, backup_id)

    importer = _Importer(conn, files, backup.files_dir, progress)
    importer.report("Checking recovery key")
    chunks = crypto.decrypt_main(backup.snapshot_dir / "main", hmac_key, aes_key, importer.set_fraction)
    frames = crypto.read_frames(chunks)
    info = pb.BackupInfo()
    info.ParseFromString(next(frames))
    if info.version != 1:
        raise crypto.BackupDecryptError(f"unsupported backup version {info.version}")
    logger.info("importing backup from app {} made at {}", info.currentAppVersion, info.backupTimeMs)

    conn.execute("BEGIN IMMEDIATE")
    try:
        for raw in frames:
            frame = pb.Frame()
            frame.ParseFromString(raw)
            importer.frame(frame)
        importer.finish()
    except BaseException:
        conn.execute("ROLLBACK")
        raise
    conn.execute("COMMIT")
    return ImportResult(
        messages_imported=importer.imported,
        messages_skipped=importer.skipped,
        attachments_imported=importer.attachments,
    )
