import sqlite3
import time

import attr


@attr.s(auto_attribs=True, frozen=True)
class ContactIds:
    aci: str | None = None
    pni: str | None = None
    e164: str | None = None

    def is_empty(self) -> bool:
        return self.aci is None and self.pni is None and self.e164 is None


def _now_ms() -> int:
    return int(time.time() * 1000)


def self_recipient_id(conn: sqlite3.Connection) -> int:
    row = conn.execute("SELECT id FROM recipients WHERE kind = 'self'").fetchone()
    if row is not None:
        return int(row["id"])
    cur = conn.execute("INSERT INTO recipients (kind, updated_at) VALUES ('self', ?)", (_now_ms(),))
    assert cur.lastrowid is not None
    return cur.lastrowid


def set_self_ids(conn: sqlite3.Connection, ids: ContactIds) -> None:
    """Record our own account identifiers, merging in any contact row that was created for them earlier (eg from a
    backup imported before registration)."""
    self_id = self_recipient_id(conn)
    for column, value in (("aci", ids.aci), ("pni", ids.pni), ("e164", ids.e164)):
        if value is None:
            continue
        other = conn.execute(f"SELECT id FROM recipients WHERE {column} = ? AND id != ?", (value, self_id)).fetchone()
        if other is not None:
            _merge_into(conn, keep_id=self_id, drop_id=int(other["id"]))
        conn.execute(f"UPDATE recipients SET {column} = ?, updated_at = ? WHERE id = ?", (value, _now_ms(), self_id))


def resolve_contact(conn: sqlite3.Connection, ids: ContactIds) -> int:
    """Find or create the recipient for a person, merging rows when `ids` shows two of them are the same person.

    The ACI is the stable identity; phone number and PNI can move between accounts, so a conflicting e164/pni is taken
    away from the old row rather than merging two different ACIs.
    """
    assert not ids.is_empty(), "contact must have at least one identifier"
    matches: dict[str, int] = {}
    for column, value in (("aci", ids.aci), ("pni", ids.pni), ("e164", ids.e164)):
        if value is None:
            continue
        row = conn.execute(f"SELECT id, aci FROM recipients WHERE {column} = ?", (value,)).fetchone()
        if row is None:
            continue
        if column != "aci" and ids.aci is not None and row["aci"] is not None and row["aci"] != ids.aci:
            conn.execute(f"UPDATE recipients SET {column} = NULL WHERE id = ?", (row["id"],))
            continue
        matches[column] = int(row["id"])

    if not matches:
        cur = conn.execute(
            "INSERT INTO recipients (kind, aci, pni, e164, updated_at) VALUES ('contact', ?, ?, ?, ?)",
            (ids.aci, ids.pni, ids.e164, _now_ms()),
        )
        assert cur.lastrowid is not None
        return cur.lastrowid

    keep_id = matches.get("aci") or matches.get("pni") or matches["e164"]
    for other_id in set(matches.values()) - {keep_id}:
        _merge_into(conn, keep_id=keep_id, drop_id=other_id)
    for column, value in (("aci", ids.aci), ("pni", ids.pni), ("e164", ids.e164)):
        if value is not None:
            conn.execute(f"UPDATE recipients SET {column} = ? WHERE id = ?", (value, keep_id))
    conn.execute("UPDATE recipients SET updated_at = ? WHERE id = ?", (_now_ms(), keep_id))
    return keep_id


def resolve_group(conn: sqlite3.Connection, group_id: str) -> int:
    row = conn.execute("SELECT id FROM recipients WHERE group_id = ?", (group_id,)).fetchone()
    if row is not None:
        return int(row["id"])
    cur = conn.execute(
        "INSERT INTO recipients (kind, group_id, updated_at) VALUES ('group', ?, ?)", (group_id, _now_ms())
    )
    assert cur.lastrowid is not None
    return cur.lastrowid


def update_contact_names(
    conn: sqlite3.Connection, recipient_id: int, profile_name: str | None, contact_name: str | None
) -> None:
    if profile_name:
        conn.execute("UPDATE recipients SET profile_name = ? WHERE id = ?", (profile_name, recipient_id))
    if contact_name:
        conn.execute("UPDATE recipients SET contact_name = ? WHERE id = ?", (contact_name, recipient_id))


def update_group(conn: sqlite3.Connection, recipient_id: int, title: str | None, master_key: str | None) -> None:
    if title:
        conn.execute("UPDATE recipients SET group_title = ? WHERE id = ?", (title, recipient_id))
    if master_key:
        conn.execute("UPDATE recipients SET group_master_key = ? WHERE id = ?", (master_key, recipient_id))


def set_group_members(conn: sqlite3.Connection, group_recipient_id: int, members: list[tuple[int, str]]) -> None:
    conn.execute("DELETE FROM group_members WHERE group_recipient_id = ?", (group_recipient_id,))
    conn.executemany(
        "INSERT OR IGNORE INTO group_members (group_recipient_id, member_recipient_id, role) VALUES (?, ?, ?)",
        [(group_recipient_id, member_id, role) for member_id, role in members],
    )


def _merge_into(conn: sqlite3.Connection, keep_id: int, drop_id: int) -> None:
    """Fold recipient `drop_id` into `keep_id`: re-point every reference, combine their 1:1 conversations, and delete
    the dropped row."""
    keep = conn.execute("SELECT * FROM recipients WHERE id = ?", (keep_id,)).fetchone()
    drop = conn.execute("SELECT * FROM recipients WHERE id = ?", (drop_id,)).fetchone()
    assert keep is not None and drop is not None

    keep_conv = conn.execute("SELECT id FROM conversations WHERE recipient_id = ?", (keep_id,)).fetchone()
    drop_conv = conn.execute("SELECT id FROM conversations WHERE recipient_id = ?", (drop_id,)).fetchone()
    if drop_conv is not None:
        if keep_conv is None:
            conn.execute("UPDATE conversations SET recipient_id = ? WHERE id = ?", (keep_id, drop_conv["id"]))
        else:
            conn.execute(
                "UPDATE messages SET conversation_id = ? WHERE conversation_id = ?", (keep_conv["id"], drop_conv["id"])
            )
            conn.execute(
                "UPDATE conversations SET last_message_at = max(last_message_at, "
                "(SELECT last_message_at FROM conversations WHERE id = ?)) WHERE id = ?",
                (drop_conv["id"], keep_conv["id"]),
            )
            conn.execute("DELETE FROM conversations WHERE id = ?", (drop_conv["id"],))

    # The same message can exist under both ids (eg seen once by phone number in a backup and once by ACI live); keep
    # the copy already attached to `keep_id`.
    conn.execute(
        "DELETE FROM messages WHERE author_id = ? AND sent_at IN (SELECT sent_at FROM messages WHERE author_id = ?)",
        (drop_id, keep_id),
    )
    conn.execute("UPDATE messages SET author_id = ? WHERE author_id = ?", (keep_id, drop_id))
    conn.execute("UPDATE messages SET quote_author_id = ? WHERE quote_author_id = ?", (keep_id, drop_id))
    conn.execute("UPDATE OR REPLACE reactions SET reactor_id = ? WHERE reactor_id = ?", (keep_id, drop_id))
    conn.execute("UPDATE OR REPLACE receipts SET recipient_id = ? WHERE recipient_id = ?", (keep_id, drop_id))
    conn.execute(
        "UPDATE OR REPLACE group_members SET member_recipient_id = ? WHERE member_recipient_id = ?", (keep_id, drop_id)
    )

    # Clear the unique identifiers before deleting so they can be copied onto `keep_id` without a conflict.
    conn.execute("UPDATE recipients SET aci = NULL, pni = NULL, e164 = NULL WHERE id = ?", (drop_id,))
    for column in ("aci", "pni", "e164", "profile_name", "contact_name", "avatar_path"):
        if keep[column] is None and drop[column] is not None:
            conn.execute(f"UPDATE recipients SET {column} = ? WHERE id = ?", (drop[column], keep_id))
    conn.execute("DELETE FROM recipients WHERE id = ?", (drop_id,))
