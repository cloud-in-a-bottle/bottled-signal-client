"""Writes encrypted local backups the way the Signal apps do, so the importer can be tested end to end."""

import os
import tempfile
from pathlib import Path

from server.backup import crypto
from server.backup import encrypt
from server.backup.proto import backup_pb2 as pb


class BackupWriter:
    def __init__(self, root: Path, recovery_key: str) -> None:
        self.root = root
        self.snapshot = root / "signal-backup-2026-10-01-12-00-00"
        self.snapshot.mkdir(parents=True)
        self.files = root / "files"
        self._bk = crypto.backup_key(recovery_key)
        self._backup_id = os.urandom(16)
        self._frames: list[bytes] = []
        info = pb.BackupInfo(version=1, backupTimeMs=1, currentAppVersion="test", firstAppVersion="test")
        self._frames.append(info.SerializeToString())

    def add(self, frame: pb.Frame) -> None:
        self._frames.append(frame.SerializeToString())

    def attachment(self, data: bytes, content_type: str, file_name: str | None = None) -> pb.FilePointer:
        with tempfile.NamedTemporaryFile() as f:
            f.write(data)
            f.flush()
            written = encrypt.write_attachment(self.files, Path(f.name))
        pointer = pb.FilePointer(contentType=content_type)
        if file_name:
            pointer.fileName = file_name
        pointer.locatorInfo.localKey = written.local_key
        pointer.locatorInfo.plaintextHash = written.plaintext_hash
        pointer.locatorInfo.size = written.size
        pointer.locatorInfo.key = os.urandom(64)
        return pointer

    def write(self) -> None:
        encrypt.write_metadata(self.snapshot / "metadata", self._bk, self._backup_id)
        hmac_key, aes_key = crypto.message_backup_keys(self._bk, self._backup_id)
        encrypt.write_main(self.snapshot / "main", hmac_key, aes_key, self._frames)
