import time

import httpx
from openhost_test_harness import OpenhostStack
from playwright.sync_api import Page
from playwright.sync_api import expect


def test_health_endpoint(stack: OpenhostStack) -> None:
    response = httpx.get(f"{stack.app_url}/health")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_signal_cli_daemon_comes_up(stack: OpenhostStack) -> None:

    deadline = time.time() + 60
    while True:
        state = stack.owner_session.get(f"{stack.url}/api/state").json()
        if state["daemon_up"] or time.time() > deadline:
            break
        time.sleep(1)
    assert state == {"number": None, "pending_number": None, "daemon_up": True}


def test_setup_page_renders(stack: OpenhostStack, page: Page) -> None:
    stack.playwright_login(page)
    page.goto(stack.url)
    expect(page.get_by_role("heading", name="Register a number")).to_be_visible()
    expect(page.get_by_label("Phone number (international format)")).to_be_visible()
