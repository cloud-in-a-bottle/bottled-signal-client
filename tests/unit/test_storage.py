import sqlite3
from pathlib import Path

from server.db import messages as dbm
from server.db import recipients as dbr
from server.db.files import FileStore
from server.db.storage import storage_report


def _attachment(sha: str | None, content_type: str, voice: bool = False) -> dbm.NewAttachment:
    return dbm.NewAttachment(
        content_type=content_type,
        filename=None,
        size=None,
        width=None,
        height=None,
        duration_ms=None,
        caption=None,
        blurhash=None,
        is_voice_note=voice,
        file_sha256=sha,
        status="present" if sha else "missing",
    )


def test_storage_report_counts_files_once_and_by_type(
    tmp_path: Path, conn: sqlite3.Connection, files: FileStore
) -> None:
    photo = files.put_bytes(b"p" * 1000)
    video = files.put_bytes(b"v" * 5000)
    voice = files.put_bytes(b"a" * 300)
    bob = dbr.resolve_contact(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000b"))
    carol = dbr.resolve_contact(conn, dbr.ContactIds(aci="00000000-0000-0000-0000-00000000000c"))

    def message(chat: int, sent_at: int, body: str | None, *attachments: dbm.NewAttachment) -> None:
        dbm.insert_message(
            conn,
            dbm.NewMessage(
                conversation_recipient_id=chat,
                author_id=chat,
                sent_at=sent_at,
                received_at=sent_at,
                server_received_at=None,
                kind="text",
                body=body,
                body_ranges=None,
                source="live",
                attachments=attachments,
            ),
        )

    message(bob, 1, "hello", _attachment(photo, "image/jpeg"), _attachment(video, "video/mp4"))
    # The same photo forwarded to another chat is stored once.
    message(carol, 2, None, _attachment(photo, "image/jpeg"), _attachment(voice, "audio/aac", voice=True))
    message(carol, 3, "héllo")
    message(carol, 4, None, _attachment(None, "application/pdf"))

    db_path = tmp_path / "test.db"
    report = storage_report(conn, files, db_path, tmp_path / "no-signal-dir")

    assert (report.messages, report.conversations, report.attachments, report.missing_attachments) == (4, 2, 5, 1)
    assert report.attachment_bytes == 6300
    by_key = {c.key: (c.count, c.bytes) for c in report.categories}
    assert by_key == {
        "images": (2, 1000),
        "videos": (1, 5000),
        "audio": (1, 300),
        "files": (0, 0),
        "text": (2, len(b"hello") + len("héllo".encode())),
    }
    assert [a.bytes for a in report.largest_attachments] == [5000, 1000, 300]
    assert [(c.name, c.bytes) for c in report.top_conversations] == [("Unknown", 6000), ("Unknown", 1300)]
