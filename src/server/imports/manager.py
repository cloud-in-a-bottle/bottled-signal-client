import asyncio
import json
import shutil
import sqlite3
import threading
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


class ImportManager:
    """Owns backup upload sessions and runs the import in a background thread.

    Uploads arrive as many small chunks (the OpenHost router buffers each request body in memory, so a multi-GB backup
    can't be one request). A session holds one or more files under relative paths, so both a single legacy `.backup`
    file and a whole backup folder can be uploaded.
    """

    def __init__(self, root: Path, conn: sqlite3.Connection, files: FileStore, broker: EventBroker) -> None:
        self._root = root / "imports"
        self._root.mkdir(parents=True, exist_ok=True)
        self._db_path = Path(conn.execute("PRAGMA database_list").fetchone()["file"])
        self._files = files
        self._broker = broker
        self._statuses: dict[str, ImportStatus] = {}
        self._lock = threading.Lock()
        self._loop: asyncio.AbstractEventLoop | None = None

    def create(self) -> ImportStatus:
        session_id = uuid.uuid4().hex
        (self._root / session_id).mkdir()
        return self._set(
            ImportStatus(
                id=session_id,
                state="uploading",
                message="Uploading",
                messages_imported=0,
                messages_skipped=0,
                attachments_imported=0,
                progress=0.0,
            )
        )

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
        if offset != size:
            # Chunks are sent in order; a retry of an already-written chunk is fine, anything else is a bug.
            if offset + len(data) <= size:
                return
            raise ValueError(f"chunk for {relative_path} at offset {offset}, but file has {size} bytes")
        with path.open("ab") as f:
            f.write(data)

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
