import hashlib
import os
import shutil
import tempfile
from collections.abc import Iterable
from pathlib import Path


class FileStore:
    """Content-addressed attachment storage: each file lives at `<root>/<sha[:2]>/<sha>`, so the same attachment from a
    backup and from a live message is stored once."""

    def __init__(self, root: Path) -> None:
        self._root = root

    def path_for(self, sha256: str) -> Path:
        return self._root / sha256[:2] / sha256

    def put_file(self, source: Path) -> str:
        """Copy `source` into the store and return its sha256."""
        with source.open("rb") as f:
            return self.put_stream(iter(lambda: f.read(1 << 20), b""))

    def put_bytes(self, data: bytes) -> str:
        return self.put_stream([data])

    def put_stream(self, chunks: Iterable[bytes]) -> str:
        digest = hashlib.sha256()
        fd, tmp_name = tempfile.mkstemp(dir=self._root, prefix=".incoming-")
        tmp = Path(tmp_name)
        try:
            with os.fdopen(fd, "wb") as out:
                for chunk in chunks:
                    digest.update(chunk)
                    out.write(chunk)
            sha = digest.hexdigest()
            dest = self.path_for(sha)
            if dest.exists():
                tmp.unlink()
            else:
                dest.parent.mkdir(exist_ok=True)
                shutil.move(tmp, dest)
            return sha
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise

    def delete(self, shas: Iterable[str]) -> None:
        for sha in shas:
            self.path_for(sha).unlink(missing_ok=True)
