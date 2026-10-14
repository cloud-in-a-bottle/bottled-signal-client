import attr
from litestar import Controller
from litestar import Request
from litestar import delete
from litestar import get
from litestar import post
from litestar import put
from litestar.datastructures import State

from server.context import AppContext
from server.imports.manager import ImportStatus


@attr.s(auto_attribs=True, frozen=True)
class StartImportRequest:
    passphrase: str


class ImportsController(Controller):
    path = "/api/imports"

    @post("/")
    async def create(self, ctx: AppContext) -> ImportStatus:
        return ctx.imports.create()

    @get("/{import_id:str}")
    async def status(self, ctx: AppContext, import_id: str) -> ImportStatus:
        return ctx.imports.status(import_id)

    @put("/{import_id:str}/chunk")
    async def chunk(
        self, ctx: AppContext, request: Request[object, object, State], import_id: str, path: str, offset: int
    ) -> None:
        ctx.imports.write_chunk(import_id, path, offset, await request.body())

    @post("/{import_id:str}/start")
    async def start(self, ctx: AppContext, import_id: str, data: StartImportRequest) -> ImportStatus:
        return ctx.imports.start(import_id, data.passphrase)

    @delete("/{import_id:str}")
    async def cancel(self, ctx: AppContext, import_id: str) -> None:
        ctx.imports.cancel(import_id)
