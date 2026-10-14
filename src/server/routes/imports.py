import attr
from litestar import Controller
from litestar import get
from litestar import post

from server.context import AppContext
from server.imports.manager import ImportStatus


@attr.s(auto_attribs=True, frozen=True)
class SpaceCheckRequest:
    total_bytes: int


@attr.s(auto_attribs=True, frozen=True)
class StartImportRequest:
    upload_id: str
    passphrase: str


class ImportsController(Controller):
    """The zip itself arrives through the tus endpoint at /api/uploads (see app.py); these start and track imports."""

    path = "/api/imports"

    @post("/check-space")
    async def check_space(self, ctx: AppContext, data: SpaceCheckRequest) -> None:
        ctx.imports.check_disk_space(data.total_bytes)

    @post("/")
    async def start(self, ctx: AppContext, data: StartImportRequest) -> ImportStatus:
        return await ctx.imports.start(data.upload_id, data.passphrase)

    @get("/{import_id:str}")
    async def status(self, ctx: AppContext, import_id: str) -> ImportStatus:
        return ctx.imports.status(import_id)
