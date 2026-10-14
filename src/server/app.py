import asyncio
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path

import attr
from litestar import Litestar
from litestar import MediaType
from litestar import Request
from litestar import Response
from litestar import get
from litestar.datastructures import State
from litestar.di import Provide
from litestar.static_files import create_static_files_router
from loguru import logger

from server.background import expire_messages_forever
from server.background import init_account_when_daemon_up
from server.config import load_config
from server.context import AppContext
from server.db.connection import connect
from server.db.connection import init_schema
from server.db.files import FileStore
from server.events import EventBroker
from server.exports.manager import ExportManager
from server.imports.manager import ImportManager
from server.routes.account import AccountController
from server.routes.chats import ChatsController
from server.routes.events import events
from server.routes.exports import ExportsController
from server.routes.imports import ImportsController
from server.signal.account_file import AccountSecretsUnavailable
from server.signal.daemon import SignalDaemon
from server.signal.receiver import Receiver
from server.signal.rpc import SignalRpc
from server.signal.rpc import SignalRpcError
from server.signal.service import SignalService
from server.signal.sse import EventStream

_STATIC_DIR = Path(__file__).parent / "static"


@attr.s(auto_attribs=True, frozen=True)
class HealthStatus:
    status: str


@attr.s(auto_attribs=True, frozen=True)
class ErrorBody:
    detail: str


@get("/health", sync_to_thread=False)
def health() -> HealthStatus:
    return HealthStatus(status="ok")


@get("/", media_type=MediaType.HTML, sync_to_thread=False)
def index() -> str:
    return (_STATIC_DIR / "index.html").read_text()


_context: AppContext | None = None


def provide_context() -> AppContext:
    assert _context is not None, "app context used before startup"
    return _context


def handle_signal_error(_: Request[object, object, State], exc: SignalRpcError) -> Response[ErrorBody]:
    logger.warning("signal-cli rejected a request: {}", exc)
    return Response(ErrorBody(detail=exc.message), status_code=400)


def handle_client_error(
    _: Request[object, object, State], exc: ValueError | AccountSecretsUnavailable
) -> Response[ErrorBody]:
    return Response(ErrorBody(detail=str(exc)), status_code=400)


@asynccontextmanager
async def lifespan(_: Litestar) -> AsyncIterator[None]:
    global _context
    config = load_config()
    conn = connect(config.db_path)
    init_schema(conn)
    files = FileStore(config.attachments_dir)
    broker = EventBroker()
    daemon = SignalDaemon(config.signal_cli_bin, config.signal_config_dir)
    rpc = SignalRpc(daemon.base_url)

    # Group updates trigger a refresh via the service, which is built after the receiver.
    def on_groups_changed() -> None:
        provide_context().signal.schedule_group_refresh()

    receiver = Receiver(conn, files, config.signal_config_dir / "attachments", broker, on_groups_changed)
    stream = EventStream(daemon.base_url, receiver.handle)

    signal = SignalService(conn, rpc, stream, files, broker, config.signal_config_dir, config.signal_cli_bin)
    imports = ImportManager(config.uploads_dir, conn, files, broker)
    exports = ExportManager(config.temp_dir, config.db_path, files, broker)
    _context = AppContext(
        config=config, conn=conn, files=files, broker=broker, signal=signal, imports=imports, exports=exports
    )

    daemon.start()
    stream.start()
    tasks = [
        asyncio.create_task(expire_messages_forever(_context)),
        asyncio.create_task(init_account_when_daemon_up(_context)),
    ]
    try:
        yield
    finally:
        for task in tasks:
            task.cancel()
        await stream.stop()
        await rpc.close()
        await daemon.stop()
        conn.close()


app = Litestar(
    route_handlers=[
        health,
        index,
        events,
        AccountController,
        ChatsController,
        ImportsController,
        ExportsController,
        create_static_files_router(path="/static", directories=[_STATIC_DIR]),
    ],
    dependencies={"ctx": Provide(provide_context, sync_to_thread=False)},
    exception_handlers={
        SignalRpcError: handle_signal_error,
        ValueError: handle_client_error,
        AccountSecretsUnavailable: handle_client_error,
    },
    lifespan=[lifespan],
    request_max_body_size=200 * 1024 * 1024,
)
