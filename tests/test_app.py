import time

import httpx
from openhost_test_harness import OpenhostStack
from playwright.sync_api import Page
from playwright.sync_api import expect


def test_health_endpoint(stack: OpenhostStack) -> None:
    response = httpx.get(f"{stack.app_url}/health")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_state_before_registering(stack: OpenhostStack) -> None:
    state = stack.owner_session.get(f"{stack.url}/api/state").json()
    assert state["account"] == {"logged_in": False, "state": "LOGGED_OUT"}
    assert state["login_step"] is None
    # signal-cli has no account yet, so there's nothing to take over: only registering.
    assert [f["id"] for f in state["login_flows"]] == ["register"]


def test_signal_cli_daemon_comes_up(stack: OpenhostStack) -> None:
    deadline = time.time() + 60
    while True:
        up = stack.owner_session.get(f"{stack.url}/api/signal-cli").json()["up"]
        if up or time.time() > deadline:
            break
        time.sleep(1)
    assert up


def test_registration_checks_the_number(stack: OpenhostStack) -> None:
    session = stack.owner_session
    step = session.post(f"{stack.url}/api/login/start", json={"flow_id": "register"})
    assert step.status_code == 200, step.text
    assert [f["id"] for f in step.json()["user_input"]["fields"]] == ["number", "method"]
    bad = session.post(f"{stack.url}/api/login/submit", json={"fields": {"number": "555", "method": "Text message"}})
    assert bad.status_code == 400
    assert "country code" in bad.json()["detail"]
    session.post(f"{stack.url}/api/login/cancel")


def test_api_requires_owner_login(stack: OpenhostStack) -> None:
    response = httpx.get(f"{stack.url}/api/conversations", follow_redirects=False)
    # The router answers unauthenticated requests to an app with no public paths as if it didn't exist.
    assert response.status_code == 404


def test_registration_steps_render(stack: OpenhostStack, page: Page) -> None:
    stack.playwright_login(page)
    page.goto(stack.url)
    expect(page.get_by_role("heading", name="Log in to Signal")).to_be_visible()
    page.get_by_role("button", name="Register a phone number").click()
    expect(page.get_by_label("Phone number")).to_be_visible()
    expect(page.get_by_label("Send the code by")).to_be_visible()
    page.get_by_role("button", name="Cancel").click()
    expect(page.get_by_role("heading", name="Log in to Signal")).to_be_visible()


def test_settings_sections_are_served(stack: OpenhostStack) -> None:
    for module in ("account.js", "import.js", "export.js", "upload.js"):
        response = stack.owner_session.get(f"{stack.url}/static/app/{module}")
        assert response.status_code == 200, module
        assert "export" in response.text


MESSAGING = "github.com/cloud-in-a-bottle/messaging-service-spec"


def test_messaging_service_through_the_router(stack: OpenhostStack) -> None:
    consumer = stack.deploy_service_consumer(MESSAGING, shortname="messaging", grants=["read"])
    stack.grant(consumer.app_id, MESSAGING, "read")
    account = consumer.call("v1/account", method="GET")
    assert account.status == 200, account.body
    assert account.body["network"] == "signal"
    assert account.body["state"] == "logged_out"
