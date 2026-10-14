import sqlite3

import attr

from server.config import Config
from server.db.files import FileStore
from server.events import EventBroker
from server.exports.manager import ExportManager
from server.imports.manager import ImportManager
from server.signal.service import SignalService


@attr.s(auto_attribs=True, frozen=True)
class AppContext:
    config: Config
    conn: sqlite3.Connection
    files: FileStore
    broker: EventBroker
    signal: SignalService
    imports: ImportManager
    exports: ExportManager
