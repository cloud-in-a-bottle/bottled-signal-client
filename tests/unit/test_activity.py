import json
import sqlite3

from server.db import messages as dbm
from server.db import queries
from server.db import recipients as dbr


def _insert(
    conn: sqlite3.Connection, chat: int, sent_at: int, body: str | None, update_type: str | None = None
) -> None:
    dbm.insert_message(
        conn,
        dbm.NewMessage(
            conversation_recipient_id=chat,
            author_id=chat,
            sent_at=sent_at,
            received_at=sent_at,
            server_received_at=None,
            kind="update" if update_type else "text",
            body=body,
            body_ranges=None,
            source="backup",
            update_payload=json.dumps({"type": update_type}) if update_type else None,
        ),
    )


def _order(conn: sqlite3.Connection) -> list[tuple[str, int, str | None]]:
    return [(c.name, c.last_message_at, c.last_message_preview) for c in queries.list_conversations(conn)]


def test_safety_number_changes_do_not_reorder_chats(conn: sqlite3.Connection) -> None:
    bob = dbr.resolve_contact(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000b"))
    carol = dbr.resolve_contact(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000c"))
    dbr.update_contact_names(conn, bob, "Bob", None)
    dbr.update_contact_names(conn, carol, "Carol", None)
    _insert(conn, bob, 100, "hi from bob")
    _insert(conn, carol, 200, "hi from carol")
    _insert(conn, bob, 300, None, update_type="safetyNumberChange")

    assert _order(conn) == [("Carol", 200, "hi from carol"), ("Bob", 100, "hi from bob")]

    # Updates that are real activity, like a missed call, still count.
    _insert(conn, bob, 400, None, update_type="call")
    assert [name for name, _, _ in _order(conn)] == ["Bob", "Carol"]


def test_recompute_fixes_chats_bumped_by_quiet_updates(conn: sqlite3.Connection) -> None:
    bob = dbr.resolve_contact(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000b"))
    _insert(conn, bob, 100, "hi")
    _insert(conn, bob, 300, None, update_type="profileChange")
    conn.execute("UPDATE conversations SET last_message_at = 300")  # as stored before quiet updates were ignored

    dbm.recompute_last_activity(conn)

    assert conn.execute("SELECT last_message_at FROM conversations").fetchone()[0] == 100
