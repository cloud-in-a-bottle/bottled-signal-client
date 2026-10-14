import os
import sqlite3
from pathlib import Path

import attr

from server.db.files import FileStore
from server.db.queries import display_name_sql

# Fixed order: it is also the order of the chart's color slots, so a category keeps its color.
_CATEGORIES = (
    ("images", "Images"),
    ("videos", "Videos"),
    ("audio", "Audio & voice notes"),
    ("files", "Other files"),
    ("text", "Message text"),
)


@attr.s(auto_attribs=True, frozen=True)
class CategoryUsage:
    key: str
    label: str
    count: int
    bytes: int


@attr.s(auto_attribs=True, frozen=True)
class ConversationUsage:
    conversation_id: int
    name: str
    messages: int
    attachments: int
    bytes: int


@attr.s(auto_attribs=True, frozen=True)
class LargeAttachment:
    attachment_id: int
    message_id: int
    conversation_id: int
    conversation_name: str
    content_type: str | None
    filename: str | None
    bytes: int
    sent_at: int


@attr.s(auto_attribs=True, frozen=True)
class StorageReport:
    messages: int
    conversations: int
    attachments: int
    missing_attachments: int
    # On-disk sizes. Attachments are deduplicated by content, so `attachment_bytes` counts each file once.
    attachment_bytes: int
    database_bytes: int
    signal_cli_bytes: int
    categories: list[CategoryUsage]
    top_conversations: list[ConversationUsage]
    largest_attachments: list[LargeAttachment]


def _category(content_type: str | None, is_voice_note: bool) -> str:
    ct = content_type or ""
    if is_voice_note or ct.startswith("audio/"):
        return "audio"
    if ct.startswith("image/"):
        return "images"
    if ct.startswith("video/"):
        return "videos"
    return "files"


def _dir_bytes(path: Path) -> int:
    total = 0
    for root, _, names in os.walk(path):
        for name in names:
            try:
                total += (Path(root) / name).lstat().st_size
            except FileNotFoundError:
                pass  # deleted while walking
    return total


def storage_report(conn: sqlite3.Connection, files: FileStore, db_path: Path, signal_dir: Path) -> StorageReport:
    """Sizes come from the files on disk rather than the attachment metadata, which can be missing or wrong."""
    file_sizes: dict[str, int] = {}
    for row in conn.execute("SELECT DISTINCT file_sha256 FROM attachments WHERE file_sha256 IS NOT NULL"):
        path = files.path_for(row[0])
        if path.exists():
            file_sizes[row[0]] = path.stat().st_size

    counts = dict.fromkeys((k for k, _ in _CATEGORIES), 0)
    sizes = dict.fromkeys((k for k, _ in _CATEGORIES), 0)
    counted: set[str] = set()
    conversation_bytes: dict[int, int] = {}
    conversation_seen: set[tuple[int, str]] = set()
    attachments = missing = 0
    for a in conn.execute(
        "SELECT a.file_sha256, a.content_type, a.is_voice_note, m.conversation_id FROM attachments a "
        "JOIN messages m ON m.id = a.message_id"
    ):
        attachments += 1
        sha = a["file_sha256"]
        if sha not in file_sizes:
            missing += 1
            continue
        category = _category(a["content_type"], bool(a["is_voice_note"]))
        counts[category] += 1
        if sha not in counted:
            counted.add(sha)
            sizes[category] += file_sizes[sha]
        if (a["conversation_id"], sha) not in conversation_seen:
            conversation_seen.add((a["conversation_id"], sha))
            conversation_bytes[a["conversation_id"]] = (
                conversation_bytes.get(a["conversation_id"], 0) + file_sizes[sha]
            )

    text = conn.execute(
        "SELECT count(*), coalesce(sum(length(CAST(body AS BLOB))), 0) FROM messages WHERE body IS NOT NULL"
    ).fetchone()
    counts["text"], sizes["text"] = int(text[0]), int(text[1])

    per_conversation = {
        r["id"]: r
        for r in conn.execute(
            f"""
            SELECT c.id, r.kind, {display_name_sql("r")} AS name,
                   (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id) AS messages,
                   (SELECT count(*) FROM attachments a JOIN messages m ON m.id = a.message_id
                     WHERE m.conversation_id = c.id) AS attachments
            FROM conversations c JOIN recipients r ON r.id = c.recipient_id
            """
        )
    }

    def conversation_name(conversation_id: int) -> str:
        row = per_conversation[conversation_id]
        return "Note to self" if row["kind"] == "self" else str(row["name"])

    top = sorted(
        (
            ConversationUsage(
                conversation_id=cid,
                name=conversation_name(cid),
                messages=row["messages"],
                attachments=row["attachments"],
                bytes=conversation_bytes.get(cid, 0),
            )
            for cid, row in per_conversation.items()
            if row["messages"]
        ),
        key=lambda c: (c.bytes, c.messages),
        reverse=True,
    )[:10]

    # One row per distinct file (its earliest message), largest first.
    largest = []
    for sha in sorted(file_sizes, key=file_sizes.__getitem__, reverse=True)[:20]:
        a = conn.execute(
            "SELECT a.id, a.content_type, a.filename, m.id AS message_id, m.conversation_id, m.sent_at "
            "FROM attachments a JOIN messages m ON m.id = a.message_id WHERE a.file_sha256 = ? "
            "ORDER BY m.sent_at LIMIT 1",
            (sha,),
        ).fetchone()
        largest.append(
            LargeAttachment(
                attachment_id=a["id"],
                message_id=a["message_id"],
                conversation_id=a["conversation_id"],
                conversation_name=conversation_name(a["conversation_id"]),
                content_type=a["content_type"],
                filename=a["filename"],
                bytes=file_sizes[sha],
                sent_at=a["sent_at"],
            )
        )

    database_bytes = sum(
        p.stat().st_size for p in (db_path, Path(f"{db_path}-wal"), Path(f"{db_path}-shm")) if p.exists()
    )
    return StorageReport(
        messages=int(conn.execute("SELECT count(*) FROM messages").fetchone()[0]),
        conversations=int(
            conn.execute(
                "SELECT count(*) FROM conversations c WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id)"
            ).fetchone()[0]
        ),
        attachments=attachments,
        missing_attachments=missing,
        attachment_bytes=sum(file_sizes.values()),
        database_bytes=database_bytes,
        signal_cli_bytes=_dir_bytes(signal_dir),
        categories=[CategoryUsage(key=k, label=label, count=counts[k], bytes=sizes[k]) for k, label in _CATEGORIES],
        top_conversations=top,
        largest_attachments=largest,
    )
