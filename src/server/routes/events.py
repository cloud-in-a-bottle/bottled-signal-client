from collections.abc import AsyncIterator

from litestar import get
from litestar.response import ServerSentEvent
from litestar.response import ServerSentEventMessage

from server.context import AppContext


@get("/api/events")
async def events(ctx: AppContext) -> ServerSentEvent:
    async def stream() -> AsyncIterator[ServerSentEventMessage]:
        yield ServerSentEventMessage(event="hello", data="{}")
        async for event in ctx.broker.subscribe(keepalive_s=15):
            if event is None:
                yield ServerSentEventMessage(comment="keepalive")
            else:
                yield ServerSentEventMessage(event=event.kind, data=event.data)

    return ServerSentEvent(stream())
