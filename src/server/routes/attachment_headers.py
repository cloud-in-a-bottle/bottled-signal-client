from typing import Literal

import attr

# Attachment types come from whoever sent the message, so only media the browser renders without running anything are
# served inline. Anything else (html, svg, xml, pdf, ...) would execute or render on this app's origin if opened, so it
# is sent as an opaque download instead.
_INLINE_TYPES = frozenset(
    {
        "image/jpeg",
        "image/png",
        "image/gif",
        "image/webp",
        "image/avif",
        "image/bmp",
        "image/heic",
        "image/heif",
        "video/mp4",
        "video/webm",
        "video/quicktime",
        "video/3gpp",
        "audio/aac",
        "audio/mpeg",
        "audio/mp4",
        "audio/x-m4a",
        "audio/ogg",
        "audio/wav",
        "audio/webm",
        "audio/flac",
    }
)

# Belt and braces for the inline case: no sniffing into a scriptable type, and a sandbox with no script or resource
# loading should the response ever be navigated to as a document.
SECURITY_HEADERS = {
    "X-Content-Type-Options": "nosniff",
    "Content-Security-Policy": "default-src 'none'; sandbox",
}


@attr.s(auto_attribs=True, frozen=True)
class AttachmentDelivery:
    media_type: str
    disposition: Literal["inline", "attachment"]


def attachment_delivery(content_type: str | None) -> AttachmentDelivery:
    essence = (content_type or "").split(";", 1)[0].strip().lower()
    if essence in _INLINE_TYPES:
        return AttachmentDelivery(media_type=essence, disposition="inline")
    return AttachmentDelivery(media_type="application/octet-stream", disposition="attachment")
