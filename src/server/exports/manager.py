import asyncio
import json
import shutil
import threading
import uuid
import zipfile
from pathlib import Path

import attr
from loguru import logger

from server.backup.exporter import export_local_backup
from server.db.connection import connect
from server.db.files import FileStore
from server.events import EventBroker
from server.events import UiEvent
from server.signal.account_file import AccountSecrets


@attr.s(auto_attribs=True, frozen=True)
class ExportStatus:
    id: str
    state: str  # exporting | done | failed
    message: str
    progress: float
    messages_exported: int
    attachments_exported: int
    size: int
    recovery_key: str | None


class ExportManager:
    """Builds a phone-restorable backup in a background thread and zips it for download. Only the latest export is
    kept: it contains the whole message history, so it shouldn't linger."""

    def __init__(self, root: Path, db_path: Path, files: FileStore, broker: EventBroker) -> None:
        self._root = root / "exports"
        shutil.rmtree(self._root, ignore_errors=True)
        self._root.mkdir(parents=True)
        self._db_path = db_path
        self._files = files
        self._broker = broker
        self._lock = threading.Lock()
        self._status: ExportStatus | None = None
        self._loop: asyncio.AbstractEventLoop | None = None

    def current(self) -> ExportStatus | None:
        with self._lock:
            return self._status

    def start(self, secrets: AccountSecrets) -> ExportStatus:
        current = self.current()
        if current is not None and current.state == "exporting":
            raise ValueError("an export is already running")
        self.discard()
        self._loop = asyncio.get_running_loop()
        status = self._set(
            ExportStatus(
                id=uuid.uuid4().hex,
                state="exporting",
                message="Starting export",
                progress=0.0,
                messages_exported=0,
                attachments_exported=0,
                size=0,
                recovery_key=None,
            )
        )
        threading.Thread(target=self._run, args=(status.id, secrets), daemon=True).start()
        return status

    def zip_path(self, export_id: str) -> Path:
        status = self.current()
        if status is None or status.id != export_id or status.state != "done":
            raise ValueError("no finished export with that id")
        return self._root / export_id / "SignalBackups.zip"

    def discard(self) -> None:
        current = self.current()
        if current is not None and current.state == "exporting":
            raise ValueError("can't discard an export that is running")
        for child in self._root.iterdir():
            shutil.rmtree(child, ignore_errors=True)
        with self._lock:
            self._status = None

    def _run(self, export_id: str, secrets: AccountSecrets) -> None:
        work = self._root / export_id
        conn = connect(self._db_path)
        try:

            def report(message: str, progress: float) -> None:
                self._update(message=message, progress=progress * 0.9)

            result = export_local_backup(conn, self._files, secrets, work, report)
            self._update(message="Packaging", progress=0.9)
            zip_path = work / "SignalBackups.zip"
            # Everything inside is already encrypted, so compressing would only cost time.
            with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_STORED, allowZip64=True) as zf:
                for path in sorted(result.backup_dir.rglob("*")):
                    if path.is_file():
                        zf.write(path, path.relative_to(work).as_posix())
            shutil.rmtree(result.backup_dir)
            self._update(
                state="done",
                message="Export ready",
                progress=1.0,
                messages_exported=result.messages_exported,
                attachments_exported=result.attachments_exported,
                size=zip_path.stat().st_size,
                recovery_key=secrets.recovery_key,
            )
        except Exception as e:
            logger.exception("backup export failed")
            shutil.rmtree(work, ignore_errors=True)
            self._update(state="failed", message=str(e) or type(e).__name__)
        finally:
            conn.close()

    def _update(self, **changes: object) -> None:
        with self._lock:
            assert self._status is not None
            status = attr.evolve(self._status, **changes)  # type: ignore[arg-type]
        self._set(status)

    def _set(self, status: ExportStatus) -> ExportStatus:
        with self._lock:
            self._status = status
        # The recovery key is only ever returned to the page that asks for the status, never broadcast.
        event = UiEvent(kind="export", data=json.dumps(attr.asdict(attr.evolve(status, recovery_key=None))))
        if self._loop is None:
            self._broker.publish(event)
        else:
            self._loop.call_soon_threadsafe(self._broker.publish, event)
        return status
