import asyncio
from collections.abc import AsyncIterator

import attr


@attr.s(auto_attribs=True, frozen=True)
class UiEvent:
    """A change notification pushed to browsers over SSE. `data` is a JSON-encoded payload."""

    kind: str
    data: str


class EventBroker:
    def __init__(self) -> None:
        self._subscribers: set[asyncio.Queue[UiEvent]] = set()

    def publish(self, event: UiEvent) -> None:
        for queue in self._subscribers:
            queue.put_nowait(event)

    async def subscribe(self, keepalive_s: float) -> AsyncIterator[UiEvent | None]:
        """Yield events as they are published, or None after `keepalive_s` of silence (for SSE keep-alives)."""
        queue: asyncio.Queue[UiEvent] = asyncio.Queue()
        self._subscribers.add(queue)
        try:
            while True:
                try:
                    yield await asyncio.wait_for(queue.get(), keepalive_s)
                except TimeoutError:
                    yield None
        finally:
            self._subscribers.discard(queue)
