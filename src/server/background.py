import asyncio
import time

from loguru import logger

from server.context import AppContext
from server.db import messages as dbm
from server.db.tx import transaction
from server.events import UiEvent


async def expire_messages_forever(ctx: AppContext) -> None:
    """Delete disappearing messages whose timers have run out."""
    while True:
        try:
            with transaction(ctx.conn):
                before = ctx.conn.total_changes
                orphaned = dbm.expire_due(ctx.conn, int(time.time() * 1000))
                changed = ctx.conn.total_changes != before
            ctx.files.delete(orphaned)
            if changed:
                ctx.broker.publish(UiEvent(kind="conversations", data="{}"))
        except Exception:
            logger.exception("message expiry failed")
        await asyncio.sleep(15)


async def init_account_when_daemon_up(ctx: AppContext) -> None:
    """On startup, wait for signal-cli to come up and then refresh our identifiers, contacts and groups."""
    if ctx.signal.account is None:
        return
    while not (await ctx.signal.state()).daemon_up:
        await asyncio.sleep(1)
    try:
        await ctx.signal.on_account_ready()
    except Exception:
        logger.exception("startup account refresh failed")
