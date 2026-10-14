import sqlite3
from collections.abc import Iterator
from pathlib import Path

import pytest

from server.db.connection import connect
from server.db.connection import init_schema
from server.db.files import FileStore


@pytest.fixture
def conn(tmp_path: Path) -> Iterator[sqlite3.Connection]:
    c = connect(tmp_path / "test.db")
    init_schema(c)
    yield c
    c.close()


@pytest.fixture
def files(tmp_path: Path) -> FileStore:
    root = tmp_path / "files"
    root.mkdir()
    return FileStore(root)
