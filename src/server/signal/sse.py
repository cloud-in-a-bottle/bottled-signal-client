import asyncio
import json
from collections.abc import Awaitable
from collections.abc import Callable

import httpx
from loguru import logger

from server.signal.rpc import JsonObject

ReceiveHandler = Callable[[JsonObject], Awaitable[None]]


class EventStream:
    """Follows the daemon's `/api/v1/events` SSE stream and hands each `receive` payload to `handler`.

    The daemon fixes the set of accounts for a stream when it opens, so `reconnect()` must be called after an account is
    registered.
    """

    def __init__(self, base_url: str, handler: ReceiveHandler) -> None:
        self._base_url = base_url
        self._handler = handler
        self._task: asyncio.Task[None] | None = None
        self._current: asyncio.Task[None] | None = None
        self._stopping = False

    def start(self) -> None:
        assert self._task is None, "event stream already started"
        self._task = asyncio.create_task(self._run_forever())

    async def stop(self) -> None:
        self._stopping = True
        if self._task is not None:
            self._task.cancel()

    def reconnect(self) -> None:
        if self._current is not None:
            self._current.cancel()

    async def _run_forever(self) -> None:
        while True:
            self._current = asyncio.create_task(self._follow())
            try:
                await self._current
            except asyncio.CancelledError:
                if self._stopping:
                    raise
                logger.info("signal event stream reconnecting")
                continue
            except (httpx.HTTPError, OSError) as e:
                logger.warning("signal event stream dropped: {}", e)
            await asyncio.sleep(2)

    async def _follow(self) -> None:
        async with (
            httpx.AsyncClient(base_url=self._base_url, timeout=httpx.Timeout(10, read=None)) as client,
            client.stream("GET", "/api/v1/events") as response,
        ):
            response.raise_for_status()
            logger.info("signal event stream connected")
            event = ""
            data_lines: list[str] = []
            async for line in response.aiter_lines():
                if line == "":
                    if event == "receive" and data_lines:
                        await self._dispatch("\n".join(data_lines))
                    event, data_lines = "", []
                elif line.startswith("event:"):
                    event = line[len("event:") :].strip()
                elif line.startswith("data:"):
                    data_lines.append(line[len("data:") :].lstrip())

    async def _dispatch(self, data: str) -> None:
        payload = json.loads(data)
        try:
            await self._handler(payload)
        except Exception:
            # One malformed envelope must not kill the stream; the raw payload is kept by the handler for re-parsing.
            logger.exception("failed to handle signal event: {}", data[:2000])
