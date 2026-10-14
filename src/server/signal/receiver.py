import json
import sqlite3
import time
from collections.abc import Callable
from pathlib import Path

from loguru import logger

from server.db import messages as dbm
from server.db import recipients as dbr
from server.db.files import FileStore
from server.db.tx import transaction
from server.events import EventBroker
from server.events import UiEvent
from server.signal.json_access import obj_list
from server.signal.json_access import opt_bool
from server.signal.json_access import opt_int
from server.signal.json_access import opt_obj
from server.signal.json_access import opt_str
from server.signal.json_access import req_int
from server.signal.rpc import JsonObject


def _now_ms() -> int:
    return int(time.time() * 1000)


def body_ranges_from_json(data: JsonObject) -> str | None:
    """Mentions and text styles, in the app's own `[{start, length, style | mentionAci}]` form."""
    ranges: list[dict[str, str | int]] = []
    for mention in obj_list(data, "mentions"):
        aci = opt_str(mention, "uuid")
        if aci is not None:
            ranges.append(
                {"start": req_int(mention, "start"), "length": req_int(mention, "length"), "mentionAci": aci}
            )
    for style in obj_list(data, "textStyles"):
        name = opt_str(style, "style")
        if name is not None:
            ranges.append({"start": req_int(style, "start"), "length": req_int(style, "length"), "style": name})
    return json.dumps(ranges) if ranges else None


class Receiver:
    """Applies signal-cli `receive` payloads to the database and notifies browsers.

    `on_groups_changed` is called when a group update arrives, so the caller can refresh group details via RPC.
    """

    def __init__(
        self,
        conn: sqlite3.Connection,
        files: FileStore,
        signal_attachments_dir: Path,
        broker: EventBroker,
        on_groups_changed: Callable[[], None],
    ) -> None:
        self._conn = conn
        self._files = files
        self._signal_attachments_dir = signal_attachments_dir
        self._broker = broker
        self._on_groups_changed = on_groups_changed

    async def handle(self, payload: JsonObject) -> None:
        exception = opt_obj(payload, "exception")
        if exception is not None:
            logger.warning("signal-cli could not process an envelope: {}", exception)
        envelope = opt_obj(payload, "envelope")
        if envelope is None:
            return

        typing = opt_obj(envelope, "typingMessage")
        if typing is not None:
            self._handle_typing(envelope, typing)
            return

        changed: set[int] = set()
        orphaned: list[str] = []
        with transaction(self._conn):
            raw_id = None
            if opt_obj(envelope, "receiptMessage") is None:
                raw_id = dbm.add_raw_event(self._conn, "live", _now_ms(), json.dumps(payload).encode())
            sender_id = self._sender(envelope)
            if sender_id is None:
                return
            data = opt_obj(envelope, "dataMessage")
            edit = opt_obj(envelope, "editMessage")
            sync = opt_obj(envelope, "syncMessage")
            receipt = opt_obj(envelope, "receiptMessage")
            if data is not None:
                orphaned += self._handle_data(envelope, sender_id, sender_id, None, data, "live", raw_id, changed)
            if edit is not None:
                self._handle_edit(sender_id, edit, raw_id, changed)
            if sync is not None:
                orphaned += self._handle_sync(envelope, sync, raw_id, changed)
            if receipt is not None:
                self._handle_receipt(sender_id, receipt, changed)
        self._files.delete(orphaned)
        for conversation_id in changed:
            self._broker.publish(UiEvent(kind="conversation", data=json.dumps({"conversationId": conversation_id})))

    def _sender(self, envelope: JsonObject) -> int | None:
        ids = dbr.ContactIds(aci=opt_str(envelope, "sourceUuid"), e164=opt_str(envelope, "sourceNumber"))
        if ids.is_empty():
            return None
        sender_id = dbr.resolve_contact(self._conn, ids)
        dbr.update_contact_names(self._conn, sender_id, opt_str(envelope, "sourceName"), None)
        return sender_id

    def _handle_typing(self, envelope: JsonObject, typing: JsonObject) -> None:
        aci = opt_str(envelope, "sourceUuid")
        group_id = opt_str(typing, "groupId")
        if group_id is not None:
            row = self._conn.execute(
                "SELECT c.id FROM conversations c JOIN recipients r ON r.id = c.recipient_id WHERE r.group_id = ?",
                (group_id,),
            ).fetchone()
        else:
            row = self._conn.execute(
                "SELECT c.id FROM conversations c JOIN recipients r ON r.id = c.recipient_id WHERE r.aci = ?", (aci,)
            ).fetchone()
        if row is None:
            return
        sender = self._conn.execute("SELECT id FROM recipients WHERE aci = ?", (aci,)).fetchone()
        self._broker.publish(
            UiEvent(
                kind="typing",
                data=json.dumps(
                    {
                        "conversationId": row["id"],
                        "recipientId": None if sender is None else sender["id"],
                        "started": opt_str(typing, "action") == "STARTED",
                    }
                ),
            )
        )

    def _conversation_recipient(self, data: JsonObject, other_party_id: int | None) -> int | None:
        group = opt_obj(data, "groupInfo")
        if group is not None:
            group_id = opt_str(group, "groupId")
            assert group_id is not None
            recipient_id = dbr.resolve_group(self._conn, group_id)
            dbr.update_group(self._conn, recipient_id, opt_str(group, "groupName"), None)
            return recipient_id
        return other_party_id

    def _target_author(self, obj: JsonObject) -> int | None:
        ids = dbr.ContactIds(aci=opt_str(obj, "targetAuthorUuid"), e164=opt_str(obj, "targetAuthorNumber"))
        return None if ids.is_empty() else dbr.resolve_contact(self._conn, ids)

    def _handle_data(
        self,
        envelope: JsonObject,
        author_id: int,
        other_party_id: int | None,
        sent_to: int | None,
        data: JsonObject,
        source: str,
        raw_id: int | None,
        changed: set[int],
    ) -> list[str]:
        """Handle a data message authored by `author_id`. For our own messages synced from a linked device,
        `other_party_id` is the 1:1 destination and `sent_to` is set (so per-recipient receipts start as 'sent')."""
        sent_at = req_int(data, "timestamp")
        conversation_recipient = self._conversation_recipient(data, other_party_id)
        if conversation_recipient is None:
            logger.warning("data message with no conversation: {}", data)
            return []

        reaction = opt_obj(data, "reaction")
        if reaction is not None:
            target_author = self._target_author(reaction)
            target = None
            if target_author is not None:
                target = dbm.find_message(self._conn, target_author, req_int(reaction, "targetSentTimestamp"))
            if target is not None:
                emoji = None if opt_bool(reaction, "isRemove") else opt_str(reaction, "emoji")
                dbm.set_reaction(self._conn, target.id, author_id, emoji, sent_at)
                changed.add(target.conversation_id)
            return []

        remote_delete = opt_obj(data, "remoteDelete")
        if remote_delete is not None:
            target = dbm.find_message(self._conn, author_id, req_int(remote_delete, "targetTimestamp"))
            if target is None:
                return []
            changed.add(target.conversation_id)
            return dbm.mark_deleted(self._conn, target.id, sent_at)

        group = opt_obj(data, "groupInfo")
        update_payload: str | None = None
        kind = "text"
        if group is not None and opt_str(group, "type") == "UPDATE":
            kind = "update"
            update_payload = json.dumps({"type": "groupUpdate", "groupName": opt_str(group, "groupName")})
            self._on_groups_changed()
        elif opt_bool(data, "isExpirationUpdate"):
            kind = "update"
            update_payload = json.dumps({"type": "expirationTimer", "seconds": opt_int(data, "expiresInSeconds") or 0})
        elif opt_obj(data, "groupCallUpdate") is not None:
            kind = "update"
            update_payload = json.dumps({"type": "groupCall"})

        body = opt_str(data, "message")
        if body is None and kind == "text":
            sticker = opt_obj(data, "sticker")
            if sticker is not None:
                body = "[sticker]"
            elif opt_obj(data, "pollCreate") is not None:
                body = "[poll]"
            elif obj_list(data, "contacts"):
                body = "[shared contact]"
            elif not obj_list(data, "attachments"):
                # Profile key updates, end-session, poll votes, pins etc. carry nothing to show.
                return []

        quote = opt_obj(data, "quote")
        quote_author = None
        if quote is not None:
            ids = dbr.ContactIds(aci=opt_str(quote, "authorUuid"), e164=opt_str(quote, "authorNumber"))
            quote_author = None if ids.is_empty() else dbr.resolve_contact(self._conn, ids)

        expire_timer = opt_int(data, "expiresInSeconds") or 0
        if kind == "update" and update_payload is not None and json.loads(update_payload)["type"] == "expirationTimer":
            conversation_id = dbm.conversation_for(self._conn, conversation_recipient)
            self._conn.execute(
                "UPDATE conversations SET expire_timer_s = ? WHERE id = ?", (expire_timer, conversation_id)
            )
            expire_timer = 0
        outgoing = sent_to is not None or source == "sent"
        stored = dbm.insert_message(
            self._conn,
            dbm.NewMessage(
                conversation_recipient_id=conversation_recipient,
                author_id=author_id,
                sent_at=sent_at,
                received_at=_now_ms(),
                server_received_at=opt_int(envelope, "serverReceivedTimestamp"),
                kind=kind,
                body=body,
                body_ranges=body_ranges_from_json(data),
                source=source,
                quote_author_id=quote_author,
                quote_sent_at=None if quote is None else opt_int(quote, "id"),
                quote_text=None if quote is None else opt_str(quote, "text"),
                expire_timer_s=expire_timer,
                # Our own messages start their timer when sent; incoming ones when we read them.
                expire_started_at=sent_at if outgoing else None,
                view_once=opt_bool(data, "viewOnce"),
                update_payload=update_payload,
                attachments=tuple(self._take_attachment(a) for a in obj_list(data, "attachments")),
            ),
        )
        if raw_id is not None:
            dbm.link_raw_event(self._conn, raw_id, stored.id)
        if sent_to is not None and stored.created:
            dbm.set_receipt(self._conn, stored.id, sent_to, "sent", sent_at)
        changed.add(stored.conversation_id)
        return []

    def _take_attachment(self, att: JsonObject) -> dbm.NewAttachment:
        """Move a downloaded attachment out of signal-cli's directory into the content-addressed store."""
        attachment_id = opt_str(att, "id")
        sha = None
        status = "missing"
        if attachment_id is not None:
            path = self._signal_attachments_dir / attachment_id
            if path.exists():
                sha = self._files.put_file(path)
                path.unlink()
                status = "present"
            else:
                logger.warning("attachment {} was not downloaded by signal-cli", attachment_id)
        return dbm.NewAttachment(
            content_type=opt_str(att, "contentType"),
            filename=opt_str(att, "filename"),
            size=opt_int(att, "size"),
            width=opt_int(att, "width"),
            height=opt_int(att, "height"),
            duration_ms=None,
            caption=opt_str(att, "caption"),
            blurhash=None,
            is_voice_note=opt_bool(att, "isVoiceNote"),
            file_sha256=sha,
            status=status,
        )

    def _handle_edit(self, author_id: int, edit: JsonObject, raw_id: int | None, changed: set[int]) -> None:
        data = opt_obj(edit, "dataMessage")
        assert data is not None
        target = dbm.find_message(self._conn, author_id, req_int(edit, "targetSentTimestamp"))
        if target is None:
            logger.info("edit for unknown message; ignoring")
            return
        dbm.apply_edit(
            self._conn, target.id, opt_str(data, "message"), body_ranges_from_json(data), req_int(data, "timestamp")
        )
        if raw_id is not None:
            dbm.link_raw_event(self._conn, raw_id, target.id)
        changed.add(target.conversation_id)

    def _handle_sync(self, envelope: JsonObject, sync: JsonObject, raw_id: int | None, changed: set[int]) -> list[str]:
        self_id = dbr.self_recipient_id(self._conn)
        orphaned: list[str] = []
        sent = opt_obj(sync, "sentMessage")
        if sent is not None:
            dest_ids = dbr.ContactIds(aci=opt_str(sent, "destinationUuid"), e164=opt_str(sent, "destinationNumber"))
            dest = None if dest_ids.is_empty() else dbr.resolve_contact(self._conn, dest_ids)
            edit = opt_obj(sent, "editMessage")
            if edit is not None:
                self._handle_edit(self_id, edit, raw_id, changed)
            else:
                orphaned += self._handle_data(envelope, self_id, dest, dest, sent, "sync", raw_id, changed)

        for read in obj_list(sync, "readMessages"):
            ids = dbr.ContactIds(aci=opt_str(read, "senderUuid"), e164=opt_str(read, "senderNumber"))
            if ids.is_empty():
                continue
            target = dbm.find_message(self._conn, dbr.resolve_contact(self._conn, ids), req_int(read, "timestamp"))
            if target is None:
                continue
            mark_read_up_to(self._conn, target.conversation_id, req_int(read, "timestamp"), _now_ms())
            changed.add(target.conversation_id)
        return orphaned

    def _handle_receipt(self, sender_id: int, receipt: JsonObject, changed: set[int]) -> None:
        if opt_bool(receipt, "isViewed"):
            status = "viewed"
        elif opt_bool(receipt, "isRead"):
            status = "read"
        elif opt_bool(receipt, "isDelivery"):
            status = "delivered"
        else:
            return
        self_id = dbr.self_recipient_id(self._conn)
        when = req_int(receipt, "when")
        timestamps = receipt.get("timestamps")
        assert isinstance(timestamps, list)
        for ts in timestamps:
            assert isinstance(ts, int)
            target = dbm.find_message(self._conn, self_id, ts)
            if target is not None:
                dbm.set_receipt(self._conn, target.id, sender_id, status, when)
                changed.add(target.conversation_id)


def mark_read_up_to(conn: sqlite3.Connection, conversation_id: int, up_to_sent_at: int, now: int) -> None:
    """Mark everything up to `up_to_sent_at` read, starting disappearing-message timers on newly read messages."""
    conn.execute(
        "UPDATE conversations SET last_read_at = max(last_read_at, ?) WHERE id = ?", (up_to_sent_at, conversation_id)
    )
    conn.execute(
        """
        UPDATE messages SET expire_started_at = ?, expires_at = ? + expire_timer_s * 1000
        WHERE conversation_id = ? AND sent_at <= ? AND expire_timer_s > 0 AND expire_started_at IS NULL
        """,
        (now, now, conversation_id, up_to_sent_at),
    )
