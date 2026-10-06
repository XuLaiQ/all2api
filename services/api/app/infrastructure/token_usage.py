"""Token counting for reported and locally estimated usage."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any

import tiktoken


def _encoding_for_model(model: str):
    try:
        return tiktoken.encoding_for_model(str(model or ""))
    except KeyError:
        try:
            return tiktoken.get_encoding("o200k_base")
        except KeyError:
            return tiktoken.get_encoding("cl100k_base")


def _content_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
        parts: list[str] = []
        for item in value:
            if isinstance(item, str):
                parts.append(item)
            elif isinstance(item, Mapping):
                text = item.get("text") or item.get("content")
                if isinstance(text, str):
                    parts.append(text)
        return "".join(parts)
    return ""


def count_text_tokens(text: str, model: str) -> int:
    return len(_encoding_for_model(model).encode(str(text or "")))


def count_message_tokens(messages: Sequence[Mapping[str, Any]], model: str) -> int:
    encoding = _encoding_for_model(model)
    total = 0
    for message in messages:
        total += 3
        for key, value in message.items():
            if key == "content":
                total += len(encoding.encode(_content_text(value)))
            elif isinstance(value, str):
                total += len(encoding.encode(value))
            if key == "name":
                total += 1
    return total + 3


def estimate_usage(
    messages: Sequence[Mapping[str, Any]] | None,
    completion: str,
    model: str,
) -> tuple[int, int]:
    prompt_tokens = count_message_tokens(messages or [], model)
    completion_tokens = count_text_tokens(completion, model)
    return prompt_tokens, completion_tokens


__all__ = ["count_message_tokens", "count_text_tokens", "estimate_usage"]
