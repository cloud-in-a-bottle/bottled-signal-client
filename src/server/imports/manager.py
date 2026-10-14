import asyncio
import json
import os
import re
import shutil
import sqlite3
import threading
import time
from pathlib import Path

import attr
from litestar_tus import FileStorageBackend
from loguru import logger

from server.backup.crypto import WrongRecoveryKey
from server.backup.importer import import_local_backup
from server.backup.importer import open_local_backup
from server.db.connection import connect
from server.db.files import FileStore
from server.events import EventBroker
from server.events import UiEvent


@attr.s(auto_attribs=True, frozen=True)
class ImportStatus:
    id: str
    state: str  # importing | done | failed
    message: str
    messages_imported: int
    messages_skipped: int
    attachments_imported: int
    progress: float
    # Set when the import failed only because of the recovery key: the upload is kept so it can be retried.
    retry_with_new_key: bool = False


# Free space kept back so an import doesn't fill the disk to the last byte.
_DISK_MARGIN_BYTES = 256 * 1024 * 1024
# Uploads that were never imported (abandoned in the browser) are removed after this long.
_STALE_UPLOAD_S = 24 * 3600
_UPLOAD_ID = re.compile(r"[0-9a-f]{32}")


class ImportManager:
    """Imports backup zips that arrived through the tus upload endpoint, one background thread per import. The uploaded
    zip is deleted once the import has finished, successfully or not: it holds the whole message history."""

    def __init__(
        self,
        uploads: FileStorageBackend,
        conn: sqlite3.Connection,
        files: FileStore,
        files_root: Path,
        broker: EventBroker,
    ) -> None:
        self._uploads = uploads
        self._upload_dir = Path(uploads.upload_dir)
        self._upload_dir.mkdir(parents=True, exist_ok=True)
        self._db_path = Path(conn.execute("PRAGMA database_list").fetchone()["file"])
        self._files = files
        self._files_root = files_root
        self._broker = broker
        self._statuses: dict[str, ImportStatus] = {}
        self._lock = threading.Lock()
        self._loop: asyncio.AbstractEventLoop | None = None
        self._remove_stale_uploads()

    def _remove_stale_uploads(self) -> None:
        now = time.time()
        for path in self._upload_dir.iterdir():
            if now - path.stat().st_mtime > _STALE_UPLOAD_S:
                logger.info("removing abandoned upload file {}", path.name)
                path.unlink(missing_ok=True)

    def check_disk_space(self, upload_bytes: int) -> None:
        """The zip is stored once, and importing writes the decrypted attachments again: up to twice its size."""
        upload_dev = os.stat(self._upload_dir).st_dev
        files_dev = os.stat(self._files_root).st_dev
        needs: dict[int, int] = {upload_dev: upload_bytes}
        needs[files_dev] = needs.get(files_dev, 0) + upload_bytes
        paths = {upload_dev: self._upload_dir, files_dev: self._files_root}
        for dev, needed in needs.items():
            free = shutil.disk_usage(paths[dev]).free
            if free < needed + _DISK_MARGIN_BYTES:
                raise ValueError(
                    f"not enough disk space: importing this backup needs about {needed / 2**30:.1f} GB free, "
                    f"but the server has {free / 2**30:.1f} GB"
                )

    def status(self, import_id: str) -> ImportStatus:
        with self._lock:
            status = self._statuses.get(import_id)
        if status is None:
            raise ValueError(f"no import {import_id}")
        return status

    async def start(self, upload_id: str, passphrase: str) -> ImportStatus:
        if not _UPLOAD_ID.fullmatch(upload_id):
            raise ValueError("bad upload id")
        try:
            info = await (await self._uploads.get_upload(upload_id)).get_info()
        except FileNotFoundError as e:
            raise ValueError("that upload doesn't exist (it may have expired); upload the zip again") from e
        if info.size is None or info.offset != info.size:
            raise ValueError("that upload hasn't finished")
        with self._lock:
            previous = self._statuses.get(upload_id)
        if previous is not None and previous.state == "importing":
            raise ValueError("that upload is already being imported")
        self._loop = asyncio.get_running_loop()
        status = self._set(
            ImportStatus(
                id=upload_id,
                state="importing",
                message="Starting import",
                messages_imported=0,
                messages_skipped=0,
                attachments_imported=0,
                progress=0.0,
            )
        )
        threading.Thread(target=self._run, args=(upload_id, passphrase), daemon=True).start()
        return status

    def _run(self, upload_id: str, passphrase: str) -> None:
        conn = connect(self._db_path)
        try:

            def report(message: str, progress: float, imported: int, skipped: int, attachments: int) -> None:
                self._set(
                    attr.evolve(
                        self.status(upload_id),
                        message=message,
                        progress=progress,
                        messages_imported=imported,
                        messages_skipped=skipped,
                        attachments_imported=attachments,
                    )
                )

            with open_local_backup(self._upload_dir / upload_id) as backup:
                result = import_local_backup(backup, passphrase, conn, self._files, report)
            self._set(
                attr.evolve(
                    self.status(upload_id),
                    state="done",
                    message="Import finished",
                    progress=1.0,
                    messages_imported=result.messages_imported,
                    messages_skipped=result.messages_skipped,
                    attachments_imported=result.attachments_imported,
                )
            )
        except WrongRecoveryKey as e:
            # Nothing was imported; keep the upload so a corrected key doesn't mean uploading it all again.
            self._set(attr.evolve(self.status(upload_id), state="failed", message=str(e), retry_with_new_key=True))
            conn.close()
            return
        except Exception as e:
            logger.exception("backup import {} failed", upload_id)
            self._set(attr.evolve(self.status(upload_id), state="failed", message=str(e) or type(e).__name__))
        conn.close()
        for suffix in ("", ".info", ".lock"):
            (self._upload_dir / f"{upload_id}{suffix}").unlink(missing_ok=True)
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
