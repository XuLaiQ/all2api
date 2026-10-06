"""Deterministic Turnstile challenge evaluator used by ChatGPT Web requests.

The upstream challenge is a small encoded instruction list.  This module only
evaluates that list and returns the derived token; it never uses account data
outside the credentials passed by the caller.
"""

from __future__ import annotations

import base64
import json
import random
import time
from typing import Any


class _OrderedMap:
    def __init__(self) -> None:
        self.keys: list[str] = []
        self.values: dict[str, Any] = {}

    def add(self, key: str, value: Any) -> None:
        if key not in self.values:
            self.keys.append(key)
        self.values[key] = value


def _value_string(value: Any) -> str:
    if value is None:
        return "undefined"
    if isinstance(value, float):
        return str(value)
    if isinstance(value, str):
        return {
            "window.Math": "[object Math]",
            "window.Reflect": "[object Reflect]",
            "window.performance": "[object Performance]",
            "window.localStorage": "[object Storage]",
            "window.Object": "function Object() { [native code] }",
            "window.Reflect.set": "function set() { [native code] }",
            "window.performance.now": "function () { [native code] }",
            "window.Object.create": "function create() { [native code] }",
            "window.Object.keys": "function keys() { [native code] }",
            "window.Math.random": "function random() { [native code] }",
        }.get(value, value)
    if isinstance(value, list) and all(isinstance(item, str) for item in value):
        return ",".join(value)
    return str(value)


def _xor_string(text: str, key: str) -> str:
    if not key:
        return text
    return "".join(chr(ord(char) ^ ord(key[index % len(key)])) for index, char in enumerate(text))


def solve_turnstile_token(dx: str, p: str) -> str | None:
    """Evaluate the provider's encoded Turnstile instruction list."""

    try:
        decoded = base64.b64decode(dx).decode()
        token_list = json.loads(_xor_string(decoded, p))
    except Exception:
        return None

    process_map: dict[Any, Any] = {}
    started = time.time()
    result = ""

    def binary_xor(target: float, source: float) -> None:
        process_map[target] = _xor_string(
            _value_string(process_map[target]), _value_string(process_map[source])
        )

    def assign(target: float, value: Any) -> None:
        process_map[target] = value

    def emit(value: str) -> None:
        nonlocal result
        result = base64.b64encode(value.encode()).decode()

    def concat(target: float, left: float) -> None:
        current = process_map[target]
        incoming = process_map[left]
        if isinstance(current, (list, tuple)):
            process_map[target] = list(current) + [incoming]
        elif isinstance(current, (str, float)) or isinstance(incoming, (str, float)):
            process_map[target] = _value_string(current) + _value_string(incoming)
        else:
            process_map[target] = "NaN"

    def join_path(target: float, left: float, right: float) -> None:
        left_value = process_map[left]
        right_value = process_map[right]
        if isinstance(left_value, str) and isinstance(right_value, str):
            value = f"{left_value}.{right_value}"
            process_map[target] = (
                "https://chatgpt.com/" if value == "window.document.location" else value
            )

    def call_reflect(target: float, *args: float) -> None:
        ref = process_map[target]
        values = [process_map[arg] for arg in args]
        if isinstance(ref, str) and ref == "window.Reflect.set":
            obj, key, value = values
            obj.add(str(key), value)
        elif callable(ref):
            ref(*values)

    def copy_value(target: float, source: float) -> None:
        process_map[target] = process_map[source]

    def parse_json(target: float, source: float) -> None:
        process_map[target] = json.loads(process_map[source])

    def stringify_json(target: float, source: float) -> None:
        process_map[target] = json.dumps(process_map[source])

    def invoke(target: float, ref_index: float, *args: float) -> None:
        call_args = [process_map[arg] for arg in args]
        ref = process_map[ref_index]
        if ref == "window.performance.now":
            process_map[target] = (time.time_ns() - int(started * 1e9) + random.random()) / 1e6
        elif ref == "window.Object.create":
            process_map[target] = _OrderedMap()
        elif ref == "window.Object.keys":
            if call_args and call_args[0] == "window.localStorage":
                process_map[target] = [
                    "STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
                    "STATSIG_LOCAL_STORAGE_STABLE_ID",
                    "client-correlated-secret",
                    "oai/apps/capExpiresAt",
                    "oai-did",
                    "STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
                    "UiState.isNavigationCollapsed.1",
                ]
        elif ref == "window.Math.random":
            process_map[target] = random.random()
        elif callable(ref):
            process_map[target] = ref(*call_args)

    def decode_base64(index: float) -> None:
        process_map[index] = base64.b64decode(_value_string(process_map[index])).decode()

    def encode_base64(index: float) -> None:
        process_map[index] = base64.b64encode(_value_string(process_map[index]).encode()).decode()

    def invoke_if_equal(left: float, right: float, ref_index: float, *args: float) -> None:
        if process_map[left] == process_map[right]:
            ref = process_map[ref_index]
            if callable(ref):
                ref(*args)

    def invoke_if_present(value_index: float, ref_index: float, *args: float) -> None:
        if process_map[value_index] is not None and callable(process_map[ref_index]):
            process_map[ref_index](*args)

    def join_strings(target: float, left: float, right: float) -> None:
        left_value = process_map[left]
        right_value = process_map[right]
        if isinstance(left_value, str) and isinstance(right_value, str):
            process_map[target] = f"{left_value}.{right_value}"

    process_map.update(
        {
            1: binary_xor,
            2: assign,
            3: emit,
            9: token_list,
            10: "window",
            14: parse_json,
            15: stringify_json,
            16: p,
            17: invoke,
            18: decode_base64,
            19: encode_base64,
            20: invoke_if_equal,
            21: lambda *_args: None,
            23: invoke_if_present,
            24: join_strings,
        }
    )

    for token in token_list:
        try:
            fn = process_map.get(token[0])
            if callable(fn):
                fn(*token[1:])
        except Exception:
            continue
    return result or None
