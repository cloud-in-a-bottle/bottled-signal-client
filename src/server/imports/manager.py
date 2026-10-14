import asyncio
import errno
import json
import os
import shutil
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from pathlib import PurePosixPath

import attr
from loguru import logger

from server.db.connection import connect
from server.db.files import FileStore
from server.events import EventBroker
from server.events import UiEvent
from server.imports.run import run_import


@attr.s(auto_attribs=True, frozen=True)
class ImportStatus:
    id: str
    state: str  # uploading | importing | done | failed
    message: str
    messages_imported: int
    messages_skipped: int
    attachments_imported: int
    progress: float


@attr.s(auto_attribs=True, frozen=True)
class UploadSession:
    """An upload that hasn't been started yet, with how much of each file has arrived, so the browser can resume it."""

    id: str
    received: dict[str, int]
    updated_at: int


# Sessions left over from an upload that was never finished are cleaned up after this long.
_ABANDONED_AFTER_S = 7 * 24 * 3600
# Free space kept back so an import doesn't fill the disk to the last byte.
_DISK_MARGIN_BYTES = 256 * 1024 * 1024


class ImportManager:
    """Owns backup upload sessions and runs the import in a background thread.

    Uploads arrive as many small chunks (the OpenHost router buffers each request body in memory, so a multi-GB backup
    can't be one request). A session holds one or more files under relative paths, so a whole backup folder can be
    uploaded. Sessions live on disk and are picked up again after a restart, so an interrupted upload can be resumed.
    """

    def __init__(
        self, root: Path, conn: sqlite3.Connection, files: FileStore, files_root: Path, broker: EventBroker
    ) -> None:
        self._root = root / "imports"
        self._root.mkdir(parents=True, exist_ok=True)
        self._db_path = Path(conn.execute("PRAGMA database_list").fetchone()["file"])
        self._files = files
        self._files_root = files_root
        self._broker = broker
        self._statuses: dict[str, ImportStatus] = {}
        self._lock = threading.Lock()
        self._loop: asyncio.AbstractEventLoop | None = None
        self._adopt_leftover_sessions()

    def _adopt_leftover_sessions(self) -> None:
        now = time.time()
        for session_dir in self._root.iterdir():
            if not session_dir.is_dir():
                continue
            if now - session_dir.stat().st_mtime > _ABANDONED_AFTER_S or not (session_dir / "files").is_dir():
                logger.info("removing abandoned upload {}", session_dir.name)
                shutil.rmtree(session_dir, ignore_errors=True)
                continue
            logger.info("found interrupted upload {}; it can be resumed", session_dir.name)
            self._statuses[session_dir.name] = self._new_status(session_dir.name, "Upload interrupted")

    @staticmethod
    def _new_status(session_id: str, message: str) -> ImportStatus:
        return ImportStatus(
            id=session_id,
            state="uploading",
            message=message,
            messages_imported=0,
            messages_skipped=0,
            attachments_imported=0,
            progress=0.0,
        )

    def create(self, total_bytes: int) -> ImportStatus:
        self._check_disk_space(total_bytes)
        session_id = uuid.uuid4().hex
        (self._root / session_id / "files").mkdir(parents=True)
        return self._set(self._new_status(session_id, "Uploading"))

    def _check_disk_space(self, upload_bytes: int) -> None:
        """The upload is stored once, and importing writes the decrypted attachments again: up to twice its size."""
        upload_dev = os.stat(self._root).st_dev
        files_dev = os.stat(self._files_root).st_dev
        needs: dict[int, int] = {upload_dev: upload_bytes}
        needs[files_dev] = needs.get(files_dev, 0) + upload_bytes
        paths = {upload_dev: self._root, files_dev: self._files_root}
        for dev, needed in needs.items():
            free = shutil.disk_usage(paths[dev]).free
            if free < needed + _DISK_MARGIN_BYTES:
                raise ValueError(
                    f"not enough disk space: importing this backup needs about {needed / 2**30:.1f} GB free, "
                    f"but the server has {free / 2**30:.1f} GB"
                )

    def resumable(self) -> list[UploadSession]:
        with self._lock:
            ids = [s.id for s in self._statuses.values() if s.state == "uploading"]
        sessions = []
        for session_id in ids:
            files_dir = self._root / session_id / "files"
            if not files_dir.is_dir():
                continue
            received = {
                p.relative_to(files_dir).as_posix(): p.stat().st_size for p in files_dir.rglob("*") if p.is_file()
            }
            sessions.append(
                UploadSession(id=session_id, received=received, updated_at=int(files_dir.stat().st_mtime * 1000))
            )
        return sorted(sessions, key=lambda s: s.updated_at, reverse=True)

    def status(self, session_id: str) -> ImportStatus:
        with self._lock:
            status = self._statuses.get(session_id)
        if status is None:
            raise ValueError(f"no import {session_id}")
        return status

    def _session_path(self, session_id: str, relative_path: str) -> Path:
        status = self.status(session_id)
        if status.state != "uploading":
            raise ValueError("this import is no longer accepting uploads")
        parts = PurePosixPath(relative_path).parts
        if (
            not parts
            or any(p in ("", ".", "..") or "/" in p or "\\" in p for p in parts)
            or relative_path.startswith("/")
        ):
            raise ValueError(f"bad upload path {relative_path!r}")
        return self._root / session_id / "files" / Path(*parts)

    def write_chunk(self, session_id: str, relative_path: str, offset: int, data: bytes) -> None:
        path = self._session_path(session_id, relative_path)
        path.parent.mkdir(parents=True, exist_ok=True)
        size = path.stat().st_size if path.exists() else 0
        if offset > size:
            logger.warning(
                "upload {}: chunk for {} at {} but only {} bytes received", session_id, relative_path, offset, size
            )
            raise ValueError(f"chunk for {relative_path} at offset {offset}, but only {size} bytes have arrived")
        if offset + len(data) <= size:
            return  # a retry of a chunk that was already written
        try:
            with path.open("r+b" if path.exists() else "wb") as f:
                # A retry after a partly-written chunk overwrites it; chunks of the same file are identical.
                f.truncate(offset)
                f.seek(offset)
                f.write(data)
        except OSError as e:
            logger.warning("upload {}: writing {} failed: {}", session_id, relative_path, e)
            if e.errno == errno.ENOSPC:
                raise ValueError("the server ran out of disk space") from e
            raise

    def start(self, session_id: str, passphrase: str) -> ImportStatus:
        status = self.status(session_id)
        if status.state != "uploading":
            raise ValueError("import already started")
        self._loop = asyncio.get_running_loop()
        status = self._set(attr.evolve(status, state="importing", message="Starting import"))
        thread = threading.Thread(target=self._run, args=(session_id, passphrase), daemon=True)
        thread.start()
        return status

    def cancel(self, session_id: str) -> None:
        status = self.status(session_id)
        if status.state == "importing":
            raise ValueError("can't cancel an import that is running")
        shutil.rmtree(self._root / session_id, ignore_errors=True)
        with self._lock:
            del self._statuses[session_id]

    def _run(self, session_id: str, passphrase: str) -> None:
        conn = connect(self._db_path)
        try:

            def report(message: str, progress: float, imported: int, skipped: int, attachments: int) -> None:
                self._set(
                    attr.evolve(
                        self.status(session_id),
                        message=message,
                        progress=progress,
                        messages_imported=imported,
                        messages_skipped=skipped,
                        attachments_imported=attachments,
                    )
                )

            result = run_import(self._root / session_id / "files", passphrase, conn, self._files, report)
            self._set(
                attr.evolve(
                    self.status(session_id),
                    state="done",
                    message="Import finished",
                    progress=1.0,
                    messages_imported=result.messages_imported,
                    messages_skipped=result.messages_skipped,
                    attachments_imported=result.attachments_imported,
                )
            )
        except Exception as e:
            logger.exception("backup import {} failed", session_id)
            self._set(attr.evolve(self.status(session_id), state="failed", message=str(e) or type(e).__name__))
        finally:
            conn.close()
            # The uploaded backup holds the whole message history; don't keep it around once imported.
            shutil.rmtree(self._root / session_id / "files", ignore_errors=True)
            self._publish(UiEvent(kind="conversations", data="{}"))

    def _set(self, status: ImportStatus) -> ImportStatus:
        with self._lock:
            self._statuses[status.id] = status
        self._publish(UiEvent(kind="import", data=json.dumps(attr.asdict(status))))
        return status

    def _publish(self, event: UiEvent) -> None:
        if self._loop is None:
            self._broker.publish(event)
        else:
            self._loop.call_soon_threadsafe(self._broker.publish, event)
