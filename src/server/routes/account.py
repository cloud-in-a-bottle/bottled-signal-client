import io
from typing import Annotated

import attr
import zxingcpp
from PIL import Image
from litestar import Controller
from litestar import delete
from litestar import get
from litestar import post
from litestar.datastructures import UploadFile
from litestar.enums import RequestEncodingType
from litestar.exceptions import ClientException
from litestar.params import Body

from server.context import AppContext
from server.signal.service import AccountState
from server.signal.service import LinkedDevice


@attr.s(auto_attribs=True, frozen=True)
class RegisterRequest:
    number: str
    captcha: str | None = None
    voice: bool = False


@attr.s(auto_attribs=True, frozen=True)
class VerifyRequest:
    code: str
    pin: str | None = None


@attr.s(auto_attribs=True, frozen=True)
class ProfileRequest:
    given_name: str
    family_name: str | None = None


@attr.s(auto_attribs=True, frozen=True)
class PinRequest:
    pin: str


@attr.s(auto_attribs=True, frozen=True)
class LinkRequest:
    uri: str


@attr.s(auto_attribs=True, frozen=True)
class DecodedQr:
    uri: str


@attr.s(auto_attribs=True, frozen=True)
class QrUpload:
    image: UploadFile


class AccountController(Controller):
    path = "/api"

    @get("/state")
    async def state(self, ctx: AppContext) -> AccountState:
        return await ctx.signal.state()

    @post("/register")
    async def register(self, ctx: AppContext, data: RegisterRequest) -> None:
        await ctx.signal.register(data.number.replace(" ", ""), data.captcha, data.voice)

    @post("/verify")
    async def verify(self, ctx: AppContext, data: VerifyRequest) -> None:
        await ctx.signal.verify(data.code, data.pin)

    @post("/profile")
    async def profile(self, ctx: AppContext, data: ProfileRequest) -> None:
        await ctx.signal.set_profile_name(data.given_name, data.family_name)

    @post("/pin")
    async def pin(self, ctx: AppContext, data: PinRequest) -> None:
        await ctx.signal.set_pin(data.pin)

    @get("/devices")
    async def devices(self, ctx: AppContext) -> list[LinkedDevice]:
        return await ctx.signal.list_devices()

    @post("/devices")
    async def link_device(self, ctx: AppContext, data: LinkRequest) -> None:
        if not data.uri.strip().startswith("sgnl://linkdevice"):
            raise ClientException("that doesn't look like a Signal device link (sgnl://linkdevice?...)")
        await ctx.signal.add_device(data.uri)

    @delete("/devices/{device_id:int}")
    async def unlink_device(self, ctx: AppContext, device_id: int) -> None:
        await ctx.signal.remove_device(device_id)

    @post("/devices/decode-qr")
    async def decode_qr(self, data: Annotated[QrUpload, Body(media_type=RequestEncodingType.MULTI_PART)]) -> DecodedQr:
        image = Image.open(io.BytesIO(await data.image.read()))
        for result in zxingcpp.read_barcodes(image):
            if result.text.startswith("sgnl://linkdevice"):
                return DecodedQr(uri=result.text)
        raise ClientException("no Signal link QR code found in that image")
