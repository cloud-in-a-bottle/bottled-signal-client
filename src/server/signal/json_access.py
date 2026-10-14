from server.signal.rpc import JsonObject
from server.signal.rpc import JsonValue


def opt_str(obj: JsonObject, key: str) -> str | None:
    value = obj.get(key)
    if value is None:
        return None
    assert isinstance(value, str), f"{key} should be a string, got {value!r}"
    return value


def opt_int(obj: JsonObject, key: str) -> int | None:
    value = obj.get(key)
    if value is None:
        return None
    assert isinstance(value, int) and not isinstance(value, bool), f"{key} should be an int, got {value!r}"
    return value


def req_int(obj: JsonObject, key: str) -> int:
    value = opt_int(obj, key)
    assert value is not None, f"missing {key}"
    return value


def opt_bool(obj: JsonObject, key: str) -> bool:
    value = obj.get(key)
    if value is None:
        return False
    assert isinstance(value, bool), f"{key} should be a bool, got {value!r}"
    return value


def opt_obj(obj: JsonObject, key: str) -> JsonObject | None:
    value = obj.get(key)
    if value is None:
        return None
    assert isinstance(value, dict), f"{key} should be an object, got {value!r}"
    return value


def obj_list(obj: JsonObject, key: str) -> list[JsonObject]:
    value = obj.get(key)
    if value is None:
        return []
    assert isinstance(value, list), f"{key} should be a list, got {value!r}"
    return [as_obj(v) for v in value]


def as_obj(value: JsonValue) -> JsonObject:
    assert isinstance(value, dict), f"expected an object, got {value!r}"
    return value


def as_list(value: JsonValue) -> list[JsonValue]:
    assert isinstance(value, list), f"expected a list, got {value!r}"
    return value
