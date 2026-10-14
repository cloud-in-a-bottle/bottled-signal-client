import sqlite3
from pathlib import Path

from server.backup.importer import ImportResult
from server.backup.importer import ProgressFn
from server.backup.importer import find_local_backup
from server.backup.importer import import_local_backup
from server.db.files import FileStore


def run_import(
    upload_dir: Path, passphrase: str, conn: sqlite3.Connection, files: FileStore, progress: ProgressFn
) -> ImportResult:
    """Import whatever backup was uploaded into `upload_dir`.

    Messages are committed in batches, so a failure part-way leaves what was imported so far; re-running is safe since
    messages are deduplicated by (author, sent timestamp).
    """
    backup = find_local_backup(upload_dir)
    return import_local_backup(backup, passphrase, conn, files, progress)
