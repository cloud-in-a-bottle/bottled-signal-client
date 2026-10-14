from litestar import Controller
from litestar import delete
from litestar import get
from litestar import post
from litestar.response import File

from server.context import AppContext
from server.exports.manager import ExportStatus


class ExportsController(Controller):
    path = "/api/export"

    @get("/")
    async def status(self, ctx: AppContext) -> ExportStatus | None:
        return ctx.exports.current()

    @post("/")
    async def start(self, ctx: AppContext) -> ExportStatus:
        return ctx.exports.start(ctx.signal.account_secrets())

    @get("/{export_id:str}/SignalBackups.zip")
    async def download(self, ctx: AppContext, export_id: str) -> File:
        return File(path=ctx.exports.zip_path(export_id), filename="SignalBackups.zip", media_type="application/zip")

    @delete("/")
    async def discard(self, ctx: AppContext) -> None:
        ctx.exports.discard()
