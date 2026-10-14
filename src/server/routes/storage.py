from litestar import get

from server.context import AppContext
from server.db.storage import StorageReport
from server.db.storage import storage_report


@get("/api/storage", sync_to_thread=False)
def storage(ctx: AppContext) -> StorageReport:
    return storage_report(ctx.conn, ctx.files, ctx.config.db_path, ctx.config.signal_config_dir)
