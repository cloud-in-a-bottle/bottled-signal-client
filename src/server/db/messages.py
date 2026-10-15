import json
import sqlite3

import attr

# Informational notices that Signal shows in a chat but that don't count as activity: they neither move the chat up the
# list nor become its preview. Matched on the `type` of a message's update_payload.
QUIET_UPDATE_TYPES = ("safetyNumberChange", "profileChange", "chatSessionRefresh")

# SQL condition (on messages aliased `m`) for messages that count as chat activity.
COUNTS_AS_ACTIVITY_SQL = (
    "NOT (m.kind = 'update' AND coalesce(json_extract(m.update_payload, '$.type'), '') IN ("
    + ", ".join(f"'{t}'" for t in QUIET_UPDATE_TYPES)
    + "))"
)


def is_quiet_update(kind: str, update_payload: str | None) -> bool:
    return (
        kind == "update"
        and update_payload is not None
        and json.loads(update_payload).get("type") in QUIET_UPDATE_TYPES
    )


def recompute_last_activity(conn: sqlite3.Connection) -> None:
    """Re-derive each chat's ordering timestamp from its messages, eg after changing what counts as activity."""
    conn.execute(
        f"""
        UPDATE conversations SET last_message_at = coalesce(
            (SELECT max(m.sent_at) FROM messages m WHERE m.conversation_id = conversations.id AND {COUNTS_AS_ACTIVITY_SQL}),
            0)
        """
    )


@attr.s(auto_attribs=True, frozen=True)
class NewAttachment:
    content_type: str | None
    filename: str | None
    size: int | None
    width: int | None
    height: int | None
    duration_ms: int | None
    caption: str | None
    blurhash: str | None
    is_voice_note: bool
    file_sha256: str | None
    status: str


@attr.s(auto_attribs=True, frozen=True)
class NewMessage:
    conversation_recipient_id: int
    author_id: int
    sent_at: int
    received_at: int
    server_received_at: int | None
    kind: str
    body: str | None
    body_ranges: str | None
    source: str
    quote_author_id: int | None = None
    quote_sent_at: int | None = None
    quote_text: str | None = None
    expire_timer_s: int = 0
    expire_started_at: int | None = None
    view_once: bool = False
    update_payload: str | None = None
    attachments: tuple[NewAttachment, ...] = ()


@attr.s(auto_attribs=True, frozen=True)
class StoredMessage:
    id: int
    conversation_id: int
    created: bool


def conversation_for(conn: sqlite3.Connection, recipient_id: int) -> int:
    row = conn.execute("SELECT id FROM conversations WHERE recipient_id = ?", (recipient_id,)).fetchone()
    if row is not None:
        return int(row["id"])
    cur = conn.execute("INSERT INTO conversations (recipient_id) VALUES (?)", (recipient_id,))
    assert cur.lastrowid is not None
    return cur.lastrowid


def insert_message(conn: sqlite3.Connection, msg: NewMessage) -> StoredMessage:
    """Insert a message unless one with the same (author, sent_at) exists, which is how live and backup copies of the
    same message are deduplicated."""
    conversation_id = conversation_for(conn, msg.conversation_recipient_id)
    existing = conn.execute(
        "SELECT id, conversation_id FROM messages WHERE author_id = ? AND sent_at = ?", (msg.author_id, msg.sent_at)
    ).fetchone()
    if existing is not None:
        return StoredMessage(id=int(existing["id"]), conversation_id=int(existing["conversation_id"]), created=False)

    expires_at = None
    if msg.expire_timer_s and msg.expire_started_at is not None:
        expires_at = msg.expire_started_at + msg.expire_timer_s * 1000
    cur = conn.execute(
        """
        INSERT INTO messages (
            conversation_id, author_id, sent_at, received_at, server_received_at, kind, body, body_ranges,
            quote_author_id, quote_sent_at, quote_text, expire_timer_s, expire_started_at, expires_at, view_once,
            update_payload, source
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            conversation_id,
            msg.author_id,
            msg.sent_at,
            msg.received_at,
            msg.server_received_at,
            msg.kind,
            msg.body,
            msg.body_ranges,
            msg.quote_author_id,
            msg.quote_sent_at,
            msg.quote_text,
            msg.expire_timer_s,
            msg.expire_started_at,
            expires_at,
            int(msg.view_once),
            msg.update_payload,
            msg.source,
        ),
    )
    message_id = cur.lastrowid
    assert message_id is not None
    for ordinal, att in enumerate(msg.attachments):
        conn.execute(
            """
            INSERT INTO attachments (
                message_id, ordinal, content_type, filename, size, width, height, duration_ms, caption, blurhash,
                is_voice_note, file_sha256, status
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                message_id,
                ordinal,
                att.content_type,
                att.filename,
                att.size,
                att.width,
                att.height,
                att.duration_ms,
                att.caption,
                att.blurhash,
                int(att.is_voice_note),
                att.file_sha256,
                att.status,
            ),
        )
    if not is_quiet_update(msg.kind, msg.update_payload):
        conn.execute(
            "UPDATE conversations SET last_message_at = max(last_message_at, ?) WHERE id = ?",
            (msg.sent_at, conversation_id),
        )
    return StoredMessage(id=message_id, conversation_id=conversation_id, created=True)


def find_message(conn: sqlite3.Connection, author_id: int, sent_at: int) -> StoredMessage | None:
    row = conn.execute(
        "SELECT id, conversation_id FROM messages WHERE author_id = ? AND sent_at = ?", (author_id, sent_at)
    ).fetchone()
    if row is None:
        return None
    return StoredMessage(id=int(row["id"]), conversation_id=int(row["conversation_id"]), created=False)


def set_reaction(conn: sqlite3.Connection, message_id: int, reactor_id: int, emoji: str | None, sent_at: int) -> None:
    if emoji is None:
        conn.execute("DELETE FROM reactions WHERE message_id = ? AND reactor_id = ?", (message_id, reactor_id))
        return
    conn.execute(
        """
        INSERT INTO reactions (message_id, reactor_id, emoji, sent_at) VALUES (?, ?, ?, ?)
        ON CONFLICT (message_id, reactor_id) DO UPDATE SET emoji = excluded.emoji, sent_at = excluded.sent_at
        WHERE excluded.sent_at >= reactions.sent_at
        """,
        (message_id, reactor_id, emoji, sent_at),
    )


_RECEIPT_RANK = {"sent": 0, "delivered": 1, "read": 2, "viewed": 3}


def set_receipt(conn: sqlite3.Connection, message_id: int, recipient_id: int, status: str, at: int) -> None:
    row = conn.execute(
        "SELECT status FROM receipts WHERE message_id = ? AND recipient_id = ?", (message_id, recipient_id)
    ).fetchone()
    if row is not None and _RECEIPT_RANK[row["status"]] >= _RECEIPT_RANK[status]:
        return
    conn.execute(
        "INSERT OR REPLACE INTO receipts (message_id, recipient_id, status, at) VALUES (?, ?, ?, ?)",
        (message_id, recipient_id, status, at),
    )


def apply_edit(
    conn: sqlite3.Connection, message_id: int, body: str | None, body_ranges: str | None, edited_at: int
) -> None:
    """Replace the message body, keeping the previous version in `message_edits`."""
    current = conn.execute(
        "SELECT body, body_ranges, sent_at, edited_at FROM messages WHERE id = ?", (message_id,)
    ).fetchone()
    assert current is not None
    if current["edited_at"] is not None and current["edited_at"] >= edited_at:
        return
    revision = conn.execute(
        "SELECT coalesce(max(revision), -1) + 1 AS r FROM message_edits WHERE message_id = ?", (message_id,)
    ).fetchone()["r"]
    conn.execute(
        "INSERT INTO message_edits (message_id, revision, body, body_ranges, edited_at) VALUES (?, ?, ?, ?, ?)",
        (message_id, revision, current["body"], current["body_ranges"], current["edited_at"] or current["sent_at"]),
    )
    conn.execute(
        "UPDATE messages SET body = ?, body_ranges = ?, edited_at = ? WHERE id = ?",
        (body, body_ranges, edited_at, message_id),
    )


def wipe_content(conn: sqlite3.Connection, message_id: int) -> list[str]:
    """Remove a message's content (remote delete, expiry), leaving a tombstone row. Returns the sha256 of each
    attachment file that is no longer referenced, for the caller to delete from disk."""
    shas = [
        r["file_sha256"]
        for r in conn.execute(
            "SELECT file_sha256 FROM attachments WHERE message_id = ? AND file_sha256 IS NOT NULL", (message_id,)
        )
    ]
    conn.execute("DELETE FROM attachments WHERE message_id = ?", (message_id,))
    conn.execute("DELETE FROM message_edits WHERE message_id = ?", (message_id,))
    conn.execute("DELETE FROM reactions WHERE message_id = ?", (message_id,))
    conn.execute("DELETE FROM raw_events WHERE message_id = ?", (message_id,))
    conn.execute(
        "UPDATE messages SET body = NULL, body_ranges = NULL, quote_text = NULL, update_payload = NULL WHERE id = ?",
        (message_id,),
    )
    return unreferenced(conn, shas)


def unreferenced(conn: sqlite3.Connection, shas: list[str]) -> list[str]:
    return [
        sha
        for sha in set(shas)
        if conn.execute("SELECT 1 FROM attachments WHERE file_sha256 = ?", (sha,)).fetchone() is None
    ]


def mark_deleted(conn: sqlite3.Connection, message_id: int, at: int) -> list[str]:
    shas = wipe_content(conn, message_id)
    conn.execute("UPDATE messages SET deleted_at = ? WHERE id = ?", (at, message_id))
    return shas


def purge_view_once(conn: sqlite3.Connection, message_id: int, at: int) -> list[str]:
    shas = [
        r["file_sha256"]
        for r in conn.execute(
            "SELECT file_sha256 FROM attachments WHERE message_id = ? AND file_sha256 IS NOT NULL", (message_id,)
        )
    ]
    conn.execute(
        "UPDATE attachments SET status = 'purged', file_sha256 = NULL, blurhash = NULL WHERE message_id = ?",
        (message_id,),
    )
    conn.execute("UPDATE messages SET viewed_at = ? WHERE id = ?", (at, message_id))
    return unreferenced(conn, shas)


def start_expiry(conn: sqlite3.Connection, message_id: int, started_at: int) -> None:
    conn.execute(
        """
        UPDATE messages SET expire_started_at = ?, expires_at = ? + expire_timer_s * 1000
        WHERE id = ? AND expire_timer_s > 0 AND expire_started_at IS NULL
        """,
        (started_at, started_at, message_id),
    )


def expire_due(conn: sqlite3.Connection, now: int) -> list[str]:
    """Hard-delete messages whose disappearing timer has run out; returns orphaned attachment shas."""
    shas: list[str] = []
    for row in conn.execute("SELECT id FROM messages WHERE expires_at IS NOT NULL AND expires_at <= ?", (now,)):
        shas.extend(wipe_content(conn, int(row["id"])))
    conn.execute("DELETE FROM messages WHERE expires_at IS NOT NULL AND expires_at <= ?", (now,))
    return unreferenced(conn, shas)


def add_raw_event(conn: sqlite3.Connection, source: str, received_at: int, payload: bytes) -> int:
    cur = conn.execute(
        "INSERT INTO raw_events (source, received_at, payload) VALUES (?, ?, ?)", (source, received_at, payload)
    )
    assert cur.lastrowid is not None
    return cur.lastrowid


def link_raw_event(conn: sqlite3.Connection, raw_id: int, message_id: int) -> None:
    conn.execute("UPDATE raw_events SET message_id = ? WHERE id = ?", (message_id, raw_id))
