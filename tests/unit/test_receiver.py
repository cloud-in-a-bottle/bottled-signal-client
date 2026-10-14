import asyncio
import sqlite3
from pathlib import Path

import pytest

from server.db import messages as dbm
from server.db import recipients as dbr
from server.db.files import FileStore
from server.events import EventBroker
from server.signal.receiver import Receiver
from server.signal.rpc import JsonObject

SELF_ACI = "00000000-0000-0000-0000-00000000000a"
SELF_NUMBER = "+15550000001"
BOB_ACI = "00000000-0000-0000-0000-00000000000b"
BOB_NUMBER = "+15550000002"
CAROL_ACI = "00000000-0000-0000-0000-00000000000c"
GROUP_ID = "Z3JvdXAtaWQ="


@pytest.fixture
def signal_attachments(tmp_path: Path) -> Path:
    d = tmp_path / "signal-attachments"
    d.mkdir()
    return d


@pytest.fixture
def receiver(conn: sqlite3.Connection, files: FileStore, signal_attachments: Path) -> Receiver:
    dbr.set_self_ids(conn, dbr.ContactIds(aci=SELF_ACI, e164=SELF_NUMBER))
    return Receiver(conn, files, signal_attachments, EventBroker(), on_groups_changed=lambda: None)


def envelope(source_aci: str, source_number: str | None, **content: JsonObject) -> JsonObject:
    env: JsonObject = {"sourceUuid": source_aci, "sourceName": "Bob B", "sourceDevice": 1, "timestamp": 1}
    if source_number is not None:
        env["sourceNumber"] = source_number
    env.update(content)
    return {"account": SELF_NUMBER, "envelope": env}


def handle(receiver: Receiver, payload: JsonObject) -> None:
    asyncio.run(receiver.handle(payload))


def messages(conn: sqlite3.Connection) -> list[sqlite3.Row]:
    return conn.execute("SELECT * FROM messages ORDER BY sent_at").fetchall()


def test_incoming_text_creates_conversation_and_contact(receiver: Receiver, conn: sqlite3.Connection) -> None:
    handle(receiver, envelope(BOB_ACI, BOB_NUMBER, dataMessage={"timestamp": 1000, "message": "hi"}))
    [msg] = messages(conn)
    assert msg["body"] == "hi"
    bob = conn.execute("SELECT * FROM recipients WHERE aci = ?", (BOB_ACI,)).fetchone()
    assert bob["e164"] == BOB_NUMBER
    assert bob["profile_name"] == "Bob B"
    assert msg["author_id"] == bob["id"]


def test_duplicate_delivery_is_ignored(receiver: Receiver, conn: sqlite3.Connection) -> None:
    for _ in range(2):
        handle(receiver, envelope(BOB_ACI, BOB_NUMBER, dataMessage={"timestamp": 1000, "message": "hi"}))
    assert len(messages(conn)) == 1


def test_reaction_edit_and_remote_delete(receiver: Receiver, conn: sqlite3.Connection) -> None:
    handle(receiver, envelope(BOB_ACI, BOB_NUMBER, dataMessage={"timestamp": 1000, "message": "helo"}))
    handle(
        receiver,
        envelope(
            BOB_ACI,
            BOB_NUMBER,
            editMessage={"targetSentTimestamp": 1000, "dataMessage": {"timestamp": 1500, "message": "hello"}},
        ),
    )
    [msg] = messages(conn)
    assert msg["body"] == "hello"
    [old] = conn.execute("SELECT * FROM message_edits").fetchall()
    assert old["body"] == "helo"

    handle(
        receiver,
        envelope(
            CAROL_ACI,
            None,
            dataMessage={
                "timestamp": 2000,
                "reaction": {
                    "emoji": "👍",
                    "targetAuthorUuid": BOB_ACI,
                    "targetSentTimestamp": 1000,
                    "isRemove": False,
                },
            },
        ),
    )
    assert conn.execute("SELECT emoji FROM reactions").fetchone()["emoji"] == "👍"
    assert len(messages(conn)) == 1, "reactions are not messages"

    handle(
        receiver,
        envelope(BOB_ACI, BOB_NUMBER, dataMessage={"timestamp": 3000, "remoteDelete": {"targetTimestamp": 1000}}),
    )
    [msg] = messages(conn)
    assert msg["deleted_at"] == 3000
    assert msg["body"] is None
    assert conn.execute("SELECT count(*) AS n FROM message_edits").fetchone()["n"] == 0
    assert conn.execute("SELECT count(*) AS n FROM reactions").fetchone()["n"] == 0


def test_group_message_and_attachment(
    receiver: Receiver, conn: sqlite3.Connection, files: FileStore, signal_attachments: Path
) -> None:
    (signal_attachments / "abc.jpg").write_bytes(b"jpegdata")
    handle(
        receiver,
        envelope(
            BOB_ACI,
            BOB_NUMBER,
            dataMessage={
                "timestamp": 1000,
                "message": None,
                "groupInfo": {"groupId": GROUP_ID, "groupName": "Fam", "revision": 1, "type": "DELIVER"},
                "attachments": [
                    {
                        "contentType": "image/jpeg",
                        "filename": "a.jpg",
                        "id": "abc.jpg",
                        "size": 8,
                        "isVoiceNote": False,
                    }
                ],
            },
        ),
    )
    [msg] = messages(conn)
    conv = conn.execute(
        "SELECT r.kind, r.group_title FROM conversations c JOIN recipients r ON r.id = c.recipient_id WHERE c.id = ?",
        (msg["conversation_id"],),
    ).fetchone()
    assert (conv["kind"], conv["group_title"]) == ("group", "Fam")
    [att] = conn.execute("SELECT * FROM attachments").fetchall()
    assert att["status"] == "present"
    assert files.path_for(att["file_sha256"]).read_bytes() == b"jpegdata"
    assert not (signal_attachments / "abc.jpg").exists()


def test_sync_sent_message_and_receipts(receiver: Receiver, conn: sqlite3.Connection) -> None:
    handle(
        receiver,
        envelope(
            SELF_ACI,
            SELF_NUMBER,
            syncMessage={
                "sentMessage": {
                    "destinationUuid": BOB_ACI,
                    "destinationNumber": BOB_NUMBER,
                    "timestamp": 1000,
                    "message": "from my phone",
                    "expiresInSeconds": 60,
                }
            },
        ),
    )
    [msg] = messages(conn)
    assert msg["author_id"] == dbr.self_recipient_id(conn)
    assert msg["source"] == "sync"
    assert msg["expires_at"] == 1000 + 60_000

    handle(
        receiver,
        envelope(
            BOB_ACI,
            BOB_NUMBER,
            receiptMessage={
                "when": 1100,
                "isDelivery": False,
                "isRead": True,
                "isViewed": False,
                "timestamps": [1000],
            },
        ),
    )
    assert conn.execute("SELECT status FROM receipts").fetchone()["status"] == "read"
    # A late delivery receipt must not downgrade a read receipt.
    handle(
        receiver,
        envelope(
            BOB_ACI,
            BOB_NUMBER,
            receiptMessage={
                "when": 1200,
                "isDelivery": True,
                "isRead": False,
                "isViewed": False,
                "timestamps": [1000],
            },
        ),
    )
    assert conn.execute("SELECT status FROM receipts").fetchone()["status"] == "read"


def test_number_only_contact_merges_when_aci_learned(receiver: Receiver, conn: sqlite3.Connection) -> None:
    # A message seen first by phone number only (eg from a backup), then live with ACI + number.
    number_only = dbr.resolve_contact(conn, dbr.ContactIds(e164=BOB_NUMBER))

    dbm.insert_message(
        conn,
        dbm.NewMessage(
            conversation_recipient_id=number_only,
            author_id=number_only,
            sent_at=500,
            received_at=500,
            server_received_at=None,
            kind="text",
            body="old",
            body_ranges=None,
            source="backup",
        ),
    )
    handle(receiver, envelope(BOB_ACI, BOB_NUMBER, dataMessage={"timestamp": 1000, "message": "new"}))
    assert conn.execute("SELECT count(*) AS n FROM recipients WHERE kind = 'contact'").fetchone()["n"] == 1
    assert conn.execute("SELECT count(*) AS n FROM conversations").fetchone()["n"] == 1
    assert [m["body"] for m in messages(conn)] == ["old", "new"]
