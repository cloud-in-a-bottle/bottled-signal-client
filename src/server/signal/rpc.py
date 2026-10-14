import itertools
import json

import httpx

# JSON-RPC results are arbitrary JSON; callers narrow them into typed objects.
JsonValue = None | bool | int | float | str | list["JsonValue"] | dict[str, "JsonValue"]
JsonObject = dict[str, JsonValue]


class SignalRpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(f"signal-cli error {code}: {message}")
        self.code = code
        self.message = message


class SignalRpc:
    def __init__(self, base_url: str) -> None:
        self._client = httpx.AsyncClient(base_url=base_url, timeout=httpx.Timeout(10, read=120))
        self._ids = itertools.count(1)

    async def close(self) -> None:
        await self._client.aclose()

    async def is_up(self) -> bool:
        try:
            response = await self._client.get("/api/v1/check")
        except httpx.TransportError:
            return False
        return response.status_code == 200

    async def call(self, method: str, params: JsonObject | None = None) -> JsonValue:
        body = {"jsonrpc": "2.0", "id": next(self._ids), "method": method, "params": params or {}}
        response = await self._client.post(
            "/api/v1/rpc", content=json.dumps(body), headers={"Content-Type": "application/json"}
        )
        response.raise_for_status()
        payload = response.json()
        if "error" in payload:
            error = payload["error"]
            raise SignalRpcError(int(error.get("code", 0)), str(error.get("message", "")))
        result: JsonValue = payload.get("result")
        return result
