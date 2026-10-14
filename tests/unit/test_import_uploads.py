import sqlite3
from pathlib import Path

import pytest

from server.db.files import FileStore
from server.events import EventBroker
from server.imports.manager import ImportManager


def _manager(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> ImportManager:
    return ImportManager(tmp_path / "uploads", conn, files, tmp_path / "files", EventBroker())


def test_interrupted_upload_resumes_after_restart(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    first = _manager(tmp_path, conn, files)
    session = first.create(total_bytes=10)
    first.write_chunk(session.id, "signal-backup-x/main", 0, b"abcd")
    first.write_chunk(session.id, "signal-backup-x/main", 4, b"ef")

    restarted = _manager(tmp_path, conn, files)
    [resumable] = restarted.resumable()
    assert resumable.id == session.id
    assert resumable.received == {"signal-backup-x/main": 6}

    # The browser resumes from the last chunk boundary it knows (4), overwriting the partly-written chunk.
    restarted.write_chunk(session.id, "signal-backup-x/main", 4, b"efgh")
    restarted.write_chunk(session.id, "signal-backup-x/main", 0, b"abcd")  # a repeated chunk is ignored
    assert (tmp_path / "uploads" / "imports" / session.id / "files" / "signal-backup-x" / "main").read_bytes() == (
        b"abcdefgh"
    )


def test_chunk_past_the_end_is_rejected(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    manager = _manager(tmp_path, conn, files)
    session = manager.create(total_bytes=10)
    with pytest.raises(ValueError, match="only 0 bytes have arrived"):
        manager.write_chunk(session.id, "main", 8, b"x")


def test_upload_paths_cannot_escape_the_session(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    manager = _manager(tmp_path, conn, files)
    session = manager.create(total_bytes=10)
    for bad in ("../escape", "/abs", "a/../../b", ""):
        with pytest.raises(ValueError, match="bad upload path"):
            manager.write_chunk(session.id, bad, 0, b"x")


def test_refuses_upload_without_disk_space(tmp_path: Path, conn: sqlite3.Connection, files: FileStore) -> None:
    manager = _manager(tmp_path, conn, files)
    with pytest.raises(ValueError, match="not enough disk space"):
        manager.create(total_bytes=2**60)
    assert manager.resumable() == []
