from pathlib import Path

from litestar import Litestar
from litestar import get
from litestar.response import File
from litestar.testing import TestClient

from server.routes.attachment_headers import SECURITY_HEADERS
from server.routes.attachment_headers import attachment_delivery


def test_scriptable_types_are_downloads() -> None:
    for content_type in (
        "text/html",
        "image/svg+xml",
        "application/xhtml+xml",
        "text/xml",
        "application/pdf",
        None,
        "",
    ):
        delivery = attachment_delivery(content_type)
        assert delivery.media_type == "application/octet-stream"
        assert delivery.disposition == "attachment"


def test_media_is_inline_with_parameters_stripped() -> None:
    delivery = attachment_delivery("Image/JPEG; charset=binary")
    assert delivery.media_type == "image/jpeg"
    assert delivery.disposition == "inline"


def test_response_headers(tmp_path: Path) -> None:
    payload = tmp_path / "x"
    payload.write_text("<script>alert(1)</script>")

    @get("/a")
    async def handler() -> File:
        delivery = attachment_delivery("text/html")
        return File(
            path=payload,
            filename="x.html",
            media_type=delivery.media_type,
            content_disposition_type=delivery.disposition,
            headers=SECURITY_HEADERS,
        )

    with TestClient(Litestar(route_handlers=[handler])) as client:
        response = client.get("/a")
    assert response.headers["content-type"] == "application/octet-stream"
    assert response.headers["content-disposition"].startswith("attachment")
    assert response.headers["x-content-type-options"] == "nosniff"
    assert "sandbox" in response.headers["content-security-policy"]
