import mimetypes
import uuid
from pathlib import Path
from typing import Annotated

import attr
from litestar import Controller
from litestar import delete
from litestar import get
from litestar import post
from litestar.datastructures import UploadFile
from litestar.enums import RequestEncodingType
from litestar.exceptions import ClientException
from litestar.exceptions import NotFoundException
from litestar.params import Body
from litestar.response import File

from server.context import AppContext
from server.db import queries
from server.db.messages import conversation_for
from server.db.tx import transaction
from server.signal.service import OutgoingAttachment


@attr.s(auto_attribs=True, frozen=True)
class NewConversationRequest:
    number: str | None = None
    recipient_id: int | None = None


@attr.s(auto_attribs=True, frozen=True)
class ConversationRef:
    id: int


@attr.s(auto_attribs=True, frozen=True)
class SendForm:
    body: str = ""
    quote_id: int | None = None
    files: list[UploadFile] = attr.Factory(list)


@attr.s(auto_attribs=True, frozen=True)
class MessageRef:
    id: int


@attr.s(auto_attribs=True, frozen=True)
class ReactionRequest:
    emoji: str | None


@attr.s(auto_attribs=True, frozen=True)
class TypingRequest:
    stop: bool


@attr.s(auto_attribs=True, frozen=True)
class MessageLocation:
    conversation_id: int
    sent_at: int


class ChatsController(Controller):
    path = "/api"

    @get("/conversations")
    async def conversations(self, ctx: AppContext) -> list[queries.ConversationSummary]:
        return queries.list_conversations(ctx.conn)

    @post("/conversations")
    async def new_conversation(self, ctx: AppContext, data: NewConversationRequest) -> ConversationRef:
        if data.recipient_id is not None:
            with transaction(ctx.conn):
                return ConversationRef(id=conversation_for(ctx.conn, data.recipient_id))
        if data.number is None:
            raise ClientException("number or recipient_id is required")
        return ConversationRef(id=await ctx.signal.lookup_number(data.number.replace(" ", "")))

    @get("/contacts")
    async def contacts(self, ctx: AppContext) -> list[queries.ConversationMember]:
        return queries.list_contacts(ctx.conn)

    @get("/conversations/{conversation_id:int}")
    async def conversation(self, ctx: AppContext, conversation_id: int) -> queries.ConversationDetail:
        detail = queries.conversation_detail(ctx.conn, conversation_id)
        if detail is None:
            raise NotFoundException()
        return detail

    @get("/conversations/{conversation_id:int}/messages")
    async def messages(
        self, ctx: AppContext, conversation_id: int, before: int | None = None, limit: int = 50
    ) -> queries.MessagePage:
        return queries.messages_page(ctx.conn, conversation_id, before, min(limit, 500))

    @post("/conversations/{conversation_id:int}/messages")
    async def send(
        self,
        ctx: AppContext,
        conversation_id: int,
        data: Annotated[SendForm, Body(media_type=RequestEncodingType.MULTI_PART)],
    ) -> MessageRef:
        attachments = []
        for upload in data.files:
            filename = Path(upload.filename or "file").name
            # signal-cli reads the file from disk and infers the name from the path, so keep the original name.
            path = ctx.config.uploads_dir / uuid.uuid4().hex / filename
            path.parent.mkdir()
            path.write_bytes(await upload.read())
            content_type = upload.content_type or mimetypes.guess_type(filename)[0] or "application/octet-stream"
            attachments.append(OutgoingAttachment(path=path, content_type=content_type, filename=filename))
        try:
            message_id = await ctx.signal.send(conversation_id, data.body.strip(), attachments, data.quote_id)
        finally:
            for a in attachments:
                a.path.unlink(missing_ok=True)
                a.path.parent.rmdir()
        return MessageRef(id=message_id)

    @post("/conversations/{conversation_id:int}/read")
    async def read(self, ctx: AppContext, conversation_id: int) -> None:
        await ctx.signal.mark_read(conversation_id)

    @post("/conversations/{conversation_id:int}/typing")
    async def typing(self, ctx: AppContext, conversation_id: int, data: TypingRequest) -> None:
        await ctx.signal.send_typing(conversation_id, data.stop)

    @post("/messages/{message_id:int}/reaction")
    async def react(self, ctx: AppContext, message_id: int, data: ReactionRequest) -> None:
        await ctx.signal.react(message_id, data.emoji)

    @delete("/messages/{message_id:int}")
    async def delete_for_everyone(self, ctx: AppContext, message_id: int) -> None:
        await ctx.signal.delete_for_everyone(message_id)

    @post("/messages/{message_id:int}/view-once-opened")
    async def view_once_opened(self, ctx: AppContext, message_id: int) -> None:
        await ctx.signal.view_once_opened(message_id)

    @get("/messages/{message_id:int}/location")
    async def location(self, ctx: AppContext, message_id: int) -> MessageLocation:
        found = queries.message_context(ctx.conn, message_id)
        if found is None:
            raise NotFoundException()
        return MessageLocation(conversation_id=found[0], sent_at=found[1])

    @get("/attachments/{attachment_id:int}")
    async def attachment(self, ctx: AppContext, attachment_id: int) -> File:
        found = queries.attachment_file(ctx.conn, attachment_id)
        if found is None:
            raise NotFoundException()
        sha, content_type, filename = found
        return File(
            path=ctx.files.path_for(sha),
            filename=filename or sha,
            media_type=content_type or "application/octet-stream",
            content_disposition_type="inline",
        )

    @get("/search")
    async def search(self, ctx: AppContext, q: str) -> list[queries.SearchHit]:
        return queries.search(ctx.conn, q, 100)
