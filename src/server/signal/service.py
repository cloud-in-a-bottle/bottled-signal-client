import asyncio
import base64
import json
import sqlite3
import time
from pathlib import Path

import attr
from loguru import logger

from server.db import kv
from server.db import messages as dbm
from server.db import recipients as dbr
from server.db.files import FileStore
from server.db.tx import transaction
from server.events import EventBroker
from server.events import UiEvent
from server.signal.account_file import AccountSecrets
from server.signal.account_file import read_account_secrets
from server.signal.account_file import read_group_master_keys
from server.signal.json_access import as_list
from server.signal.json_access import as_obj
from server.signal.json_access import obj_list
from server.signal.json_access import opt_bool
from server.signal.json_access import opt_int
from server.signal.json_access import opt_obj
from server.signal.json_access import opt_str
from server.signal.json_access import req_int
from server.signal.receiver import mark_read_up_to
from server.signal.rpc import JsonObject
from server.signal.rpc import JsonValue
from server.signal.rpc import SignalRpc
from server.signal.sse import EventStream

_ACCOUNT_KEY = "account_number"
_PENDING_KEY = "pending_registration_number"


@attr.s(auto_attribs=True, frozen=True)
class AccountState:
    number: str | None
    pending_number: str | None
    daemon_up: bool


@attr.s(auto_attribs=True, frozen=True)
class LinkedDevice:
    id: int
    name: str | None
    created_at: int | None
    last_seen_at: int | None


@attr.s(auto_attribs=True, frozen=True)
class OutgoingAttachment:
    path: Path
    content_type: str
    filename: str


def _now_ms() -> int:
    return int(time.time() * 1000)


class SignalService:
    def __init__(
        self,
        conn: sqlite3.Connection,
        rpc: SignalRpc,
        stream: EventStream,
        files: FileStore,
        broker: EventBroker,
        signal_data_dir: Path,
    ) -> None:
        self._signal_data_dir = signal_data_dir
        self._conn = conn
        self._rpc = rpc
        self._stream = stream
        self._files = files
        self._broker = broker
        self._group_refresh: asyncio.Task[None] | None = None

    @property
    def account(self) -> str | None:
        return kv.get(self._conn, _ACCOUNT_KEY)

    def _require_account(self) -> str:
        account = self.account
        if account is None:
            raise RuntimeError("no Signal account registered yet")
        return account

    async def _call(self, method: str, params: JsonObject) -> JsonValue:
        return await self._rpc.call(method, {"account": self._require_account(), **params})

    async def state(self) -> AccountState:
        return AccountState(
            number=self.account, pending_number=kv.get(self._conn, _PENDING_KEY), daemon_up=await self._rpc.is_up()
        )

    # --- registration -------------------------------------------------------------------------------------------

    async def register(self, number: str, captcha: str | None, voice: bool) -> None:
        params: JsonObject = {"account": number, "voice": voice}
        if captcha:
            params["captcha"] = captcha.strip()
        await self._rpc.call("register", params)
        with transaction(self._conn):
            kv.put(self._conn, _PENDING_KEY, number)

    async def verify(self, code: str, pin: str | None) -> None:
        number = kv.get(self._conn, _PENDING_KEY)
        if number is None:
            raise RuntimeError("no registration in progress")
        params: JsonObject = {"account": number, "verificationCode": code.replace("-", "").strip()}
        if pin:
            params["pin"] = pin
        await self._rpc.call("verify", params)
        with transaction(self._conn):
            kv.put(self._conn, _ACCOUNT_KEY, number)
            kv.delete(self._conn, _PENDING_KEY)
        await self.on_account_ready()

    async def on_account_ready(self) -> None:
        """Record our own identifiers and pull contacts/groups. Run after registration and on every startup."""
        number = self._require_account()
        aci = None
        for entry in as_list(await self._rpc.call("listAccounts")):
            account = as_obj(entry)
            if opt_str(account, "number") == number:
                aci = opt_str(account, "aci") or opt_str(account, "uuid")
        with transaction(self._conn):
            dbr.set_self_ids(self._conn, dbr.ContactIds(aci=aci, e164=number))
        self._stream.reconnect()
        await self.refresh_contacts()
        await self.refresh_groups()

    async def set_profile_name(self, given_name: str, family_name: str | None) -> None:
        params: JsonObject = {"givenName": given_name}
        if family_name:
            params["familyName"] = family_name
        await self._call("updateProfile", params)

    async def set_pin(self, pin: str) -> None:
        await self._call("setPin", {"pin": pin})

    # --- linked devices -----------------------------------------------------------------------------------------

    async def add_device(self, uri: str) -> None:
        await self._call("addDevice", {"uri": uri.strip()})

    async def list_devices(self) -> list[LinkedDevice]:
        devices = []
        for entry in as_list(await self._call("listDevices", {})):
            d = as_obj(entry)
            devices.append(
                LinkedDevice(
                    id=req_int(d, "id"),
                    name=opt_str(d, "name"),
                    created_at=opt_int(d, "createdTimestamp"),
                    last_seen_at=opt_int(d, "lastSeenTimestamp"),
                )
            )
        return devices

    async def remove_device(self, device_id: int) -> None:
        await self._call("removeDevice", {"deviceId": device_id})

    # --- contacts and groups ------------------------------------------------------------------------------------

    async def refresh_contacts(self) -> None:
        contacts = as_list(await self._call("listContacts", {"allRecipients": True}))
        with transaction(self._conn):
            for entry in contacts:
                c = as_obj(entry)
                ids = dbr.ContactIds(aci=opt_str(c, "uuid"), e164=opt_str(c, "number"))
                if ids.is_empty():
                    continue
                recipient_id = dbr.resolve_contact(self._conn, ids)
                profile = opt_obj(c, "profile")
                profile_name = None
                if profile is not None:
                    profile_name = " ".join(
                        n for n in (opt_str(profile, "givenName"), opt_str(profile, "familyName")) if n
                    )
                dbr.update_contact_names(self._conn, recipient_id, profile_name or None, opt_str(c, "name"))
                timer = opt_int(c, "messageExpirationTime")
                if timer is not None:
                    self._conn.execute(
                        "UPDATE conversations SET expire_timer_s = ? WHERE recipient_id = ?", (timer, recipient_id)
                    )
        self._broker.publish(UiEvent(kind="conversations", data="{}"))

    def account_secrets(self) -> AccountSecrets:
        return read_account_secrets(self._signal_data_dir, self._require_account())

    async def refresh_groups(self) -> None:
        groups = as_list(await self._call("listGroups", {}))
        master_keys = read_group_master_keys(self._signal_data_dir, self._require_account())
        with transaction(self._conn):
            for entry in groups:
                g = as_obj(entry)
                group_id = opt_str(g, "id")
                if group_id is None:
                    continue
                recipient_id = dbr.resolve_group(self._conn, group_id)
                master_key = master_keys.get(group_id)
                dbr.update_group(
                    self._conn,
                    recipient_id,
                    opt_str(g, "name"),
                    None if master_key is None else base64.b64encode(master_key).decode(),
                )
                admins = {opt_str(a, "uuid") or opt_str(a, "number") for a in obj_list(g, "admins")}
                members: list[tuple[int, str]] = []
                for m in obj_list(g, "members"):
                    ids = dbr.ContactIds(aci=opt_str(m, "uuid"), e164=opt_str(m, "number"))
                    if ids.is_empty():
                        continue
                    role = "admin" if (ids.aci or ids.e164) in admins else "member"
                    members.append((dbr.resolve_contact(self._conn, ids), role))
                dbr.set_group_members(self._conn, recipient_id, members)
                if opt_bool(g, "isMember"):
                    dbm.conversation_for(self._conn, recipient_id)
                timer = opt_int(g, "messageExpirationTime")
                if timer is not None:
                    self._conn.execute(
                        "UPDATE conversations SET expire_timer_s = ? WHERE recipient_id = ?", (timer, recipient_id)
                    )
        self._broker.publish(UiEvent(kind="conversations", data="{}"))

    def schedule_group_refresh(self) -> None:
        if self._group_refresh is not None and not self._group_refresh.done():
            return

        async def run() -> None:
            await asyncio.sleep(1)
            try:
                await self.refresh_groups()
            except Exception:
                logger.exception("group refresh failed")

        self._group_refresh = asyncio.create_task(run())

    async def lookup_number(self, number: str) -> int:
        """Find (or create) the conversation for a phone number, confirming it is registered on Signal."""
        statuses = as_list(await self._call("getUserStatus", {"recipient": [number]}))
        status = as_obj(statuses[0])
        if not opt_bool(status, "isRegistered"):
            raise ValueError(f"{number} is not registered on Signal")
        with transaction(self._conn):
            recipient_id = dbr.resolve_contact(
                self._conn, dbr.ContactIds(aci=opt_str(status, "uuid"), e164=opt_str(status, "number") or number)
            )
            return dbm.conversation_for(self._conn, recipient_id)

    # --- messaging ----------------------------------------------------------------------------------------------

    def _target_params(self, conversation_id: int) -> JsonObject:
        row = self._conn.execute(
            "SELECT r.kind, r.aci, r.e164, r.group_id FROM conversations c JOIN recipients r ON r.id = c.recipient_id "
            "WHERE c.id = ?",
            (conversation_id,),
        ).fetchone()
        if row is None:
            raise ValueError(f"no conversation {conversation_id}")
        if row["kind"] == "group":
            return {"groupId": row["group_id"]}
        if row["kind"] == "self":
            return {"noteToSelf": True}
        return {"recipient": [row["aci"] or row["e164"]]}

    def _address(self, recipient_id: int) -> str:
        row = self._conn.execute("SELECT aci, e164, kind FROM recipients WHERE id = ?", (recipient_id,)).fetchone()
        assert row is not None
        if row["kind"] == "self":
            return self._require_account()
        address = row["aci"] or row["e164"]
        assert address is not None, f"recipient {recipient_id} has no address"
        return str(address)

    async def send(
        self,
        conversation_id: int,
        body: str,
        attachments: list[OutgoingAttachment],
        quote_message_id: int | None,
    ) -> int:
        params = self._target_params(conversation_id)
        if body:
            params["message"] = body
        if attachments:
            params["attachments"] = [str(a.path) for a in attachments]
        quote = None
        if quote_message_id is not None:
            quote = self._conn.execute(
                "SELECT author_id, sent_at, body FROM messages WHERE id = ?", (quote_message_id,)
            ).fetchone()
            assert quote is not None
            params["quoteTimestamp"] = quote["sent_at"]
            params["quoteAuthor"] = self._address(quote["author_id"])
            params["quoteMessage"] = quote["body"] or ""
        conv = self._conn.execute(
            "SELECT expire_timer_s FROM conversations WHERE id = ?", (conversation_id,)
        ).fetchone()

        result = as_obj(await self._call("send", params))
        sent_at = req_int(result, "timestamp")

        stored_attachments = []
        for a in attachments:
            sha = self._files.put_file(a.path)
            a.path.unlink()
            stored_attachments.append(
                dbm.NewAttachment(
                    content_type=a.content_type,
                    filename=a.filename,
                    size=self._files.path_for(sha).stat().st_size,
                    width=None,
                    height=None,
                    duration_ms=None,
                    caption=None,
                    blurhash=None,
                    is_voice_note=False,
                    file_sha256=sha,
                    status="present",
                )
            )
        with transaction(self._conn):
            self_id = dbr.self_recipient_id(self._conn)
            recipient_id = self._conn.execute(
                "SELECT recipient_id FROM conversations WHERE id = ?", (conversation_id,)
            ).fetchone()["recipient_id"]
            stored = dbm.insert_message(
                self._conn,
                dbm.NewMessage(
                    conversation_recipient_id=recipient_id,
                    author_id=self_id,
                    sent_at=sent_at,
                    received_at=_now_ms(),
                    server_received_at=None,
                    kind="text",
                    body=body or None,
                    body_ranges=None,
                    source="sent",
                    quote_author_id=None if quote is None else quote["author_id"],
                    quote_sent_at=None if quote is None else quote["sent_at"],
                    quote_text=None if quote is None else quote["body"],
                    expire_timer_s=conv["expire_timer_s"],
                    expire_started_at=sent_at,
                    attachments=tuple(stored_attachments),
                ),
            )
            for entry in obj_list(result, "results"):
                address = opt_obj(entry, "recipientAddress")
                if address is None:
                    continue
                ids = dbr.ContactIds(aci=opt_str(address, "uuid"), e164=opt_str(address, "number"))
                if ids.is_empty():
                    continue
                status = "sent" if opt_str(entry, "type") == "SUCCESS" else None
                if status is not None:
                    dbm.set_receipt(self._conn, stored.id, dbr.resolve_contact(self._conn, ids), status, sent_at)
                else:
                    logger.warning("send to {} failed: {}", ids, opt_str(entry, "type"))
        self._publish(conversation_id)
        return stored.id

    async def react(self, message_id: int, emoji: str | None) -> None:
        msg = self._message(message_id)
        params = self._target_params(msg["conversation_id"])
        params.update(
            {
                "emoji": emoji or "",
                "targetAuthor": self._address(msg["author_id"]),
                "targetTimestamp": msg["sent_at"],
                "remove": emoji is None,
            }
        )
        if emoji is None:
            existing = self._conn.execute(
                "SELECT emoji FROM reactions WHERE message_id = ? AND reactor_id = ?",
                (message_id, dbr.self_recipient_id(self._conn)),
            ).fetchone()
            if existing is None:
                return
            params["emoji"] = existing["emoji"]
        await self._call("sendReaction", params)
        with transaction(self._conn):
            dbm.set_reaction(self._conn, message_id, dbr.self_recipient_id(self._conn), emoji, _now_ms())
        self._publish(msg["conversation_id"])

    async def delete_for_everyone(self, message_id: int) -> None:
        msg = self._message(message_id)
        if msg["author_id"] != dbr.self_recipient_id(self._conn):
            raise ValueError("can only delete your own messages for everyone")
        params = self._target_params(msg["conversation_id"])
        params["targetTimestamp"] = msg["sent_at"]
        await self._call("remoteDelete", params)
        with transaction(self._conn):
            orphaned = dbm.mark_deleted(self._conn, message_id, _now_ms())
        self._files.delete(orphaned)
        self._publish(msg["conversation_id"])

    async def mark_read(self, conversation_id: int) -> None:
        """Mark a conversation read, send read receipts to the authors, and start disappearing timers."""
        conv = self._conn.execute(
            "SELECT last_read_at, last_message_at FROM conversations WHERE id = ?", (conversation_id,)
        ).fetchone()
        if conv is None or conv["last_message_at"] <= conv["last_read_at"]:
            return
        self_id = dbr.self_recipient_id(self._conn)
        unread = self._conn.execute(
            "SELECT author_id, sent_at FROM messages WHERE conversation_id = ? AND sent_at > ? AND author_id != ? "
            "AND kind = 'text'",
            (conversation_id, conv["last_read_at"], self_id),
        ).fetchall()
        with transaction(self._conn):
            mark_read_up_to(self._conn, conversation_id, conv["last_message_at"], _now_ms())
        by_author: dict[int, list[JsonValue]] = {}
        for row in unread:
            by_author.setdefault(row["author_id"], []).append(row["sent_at"])
        for author_id, timestamps in by_author.items():
            try:
                await self._call(
                    "sendReceipt",
                    {"recipient": self._address(author_id), "targetTimestamp": timestamps, "type": "read"},
                )
            except Exception:
                logger.exception("failed to send read receipt")
        self._broker.publish(UiEvent(kind="conversations", data="{}"))

    async def view_once_opened(self, message_id: int) -> None:
        msg = self._message(message_id)
        with transaction(self._conn):
            orphaned = dbm.purge_view_once(self._conn, message_id, _now_ms())
        self._files.delete(orphaned)
        if msg["author_id"] != dbr.self_recipient_id(self._conn):
            try:
                await self._call(
                    "sendReceipt",
                    {
                        "recipient": self._address(msg["author_id"]),
                        "targetTimestamp": [msg["sent_at"]],
                        "type": "viewed",
                    },
                )
            except Exception:
                logger.exception("failed to send viewed receipt")
        self._publish(msg["conversation_id"])

    async def send_typing(self, conversation_id: int, stop: bool) -> None:
        params = self._target_params(conversation_id)
        if "noteToSelf" in params:
            return
        params["stop"] = stop
        await self._call("sendTyping", params)

    def _message(self, message_id: int) -> sqlite3.Row:
        row = self._conn.execute(
            "SELECT id, conversation_id, author_id, sent_at FROM messages WHERE id = ?", (message_id,)
        ).fetchone()
        if row is None:
            raise ValueError(f"no message {message_id}")
        return row  # type: ignore[no-any-return]

    def _publish(self, conversation_id: int) -> None:
        self._broker.publish(UiEvent(kind="conversation", data=json.dumps({"conversationId": conversation_id})))
