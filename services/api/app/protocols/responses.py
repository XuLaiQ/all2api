from __future__ import annotations

import json
import time
import uuid
from collections.abc import AsyncIterator
from typing import Any


class ResponsesRequestError(ValueError):
    pass


def _content_text(value: Any, field: str) -> str:
    if isinstance(value, str):
        return value
    if not isinstance(value, list):
        raise ResponsesRequestError(f"{field} must be text or an array of input_text")
    parts = []
    for index, item in enumerate(value):
        if (
            not isinstance(item, dict)
            or item.get("type") != "input_text"
            or not isinstance(item.get("text"), str)
        ):
            raise ResponsesRequestError(f"{field}[{index}] only supports input_text")
        parts.append(item["text"])
    return "".join(parts)


def normalize_responses_request(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise ResponsesRequestError("request body must be a JSON object")
    allowed = {
        "model",
        "input",
        "instructions",
        "max_output_tokens",
        "temperature",
        "top_p",
        "stream",
        "metadata",
        "store",
    }
    unsupported = sorted(set(payload) - allowed)
    if unsupported:
        raise ResponsesRequestError(f"unsupported request field: {unsupported[0]}")
    model = payload.get("model")
    if not isinstance(model, str) or not model.strip():
        raise ResponsesRequestError("model is required")
    stream = payload.get("stream", False)
    if not isinstance(stream, bool):
        raise ResponsesRequestError("stream must be a boolean")
    if payload.get("store", False) is not False:
        raise ResponsesRequestError("store=true is not supported")
    if "metadata" in payload and (
        not isinstance(payload["metadata"], dict)
        or any(
            not isinstance(key, str) or not isinstance(value, str)
            for key, value in payload["metadata"].items()
        )
    ):
        raise ResponsesRequestError("metadata must contain string values")
    result: dict[str, Any] = {
        "model": model,
        "messages": [],
        "stream": stream,
    }
    max_output_tokens = payload.get("max_output_tokens")
    if max_output_tokens is not None:
        if type(max_output_tokens) is not int or max_output_tokens < 1:
            raise ResponsesRequestError("max_output_tokens must be a positive integer")
        result["max_tokens"] = max_output_tokens
    instructions = payload.get("instructions")
    if instructions is not None:
        if not isinstance(instructions, str):
            raise ResponsesRequestError("instructions must be a string")
        result["messages"].append({"role": "system", "content": instructions})
    source_input = payload.get("input")
    if isinstance(source_input, str):
        result["messages"].append({"role": "user", "content": source_input})
    elif isinstance(source_input, list) and source_input:
        for index, item in enumerate(source_input):
            field = f"input[{index}]"
            if not isinstance(item, dict):
                raise ResponsesRequestError(f"{field} must be an object")
            if item.get("type", "message") != "message":
                raise ResponsesRequestError(f"{field} only supports message input items")
            role = item.get("role")
            if role not in {"system", "developer", "user", "assistant"}:
                raise ResponsesRequestError(f"{field}.role is not supported")
            content = _content_text(item.get("content"), f"{field}.content")
            result["messages"].append(
                {"role": "system" if role == "developer" else role, "content": content}
            )
    else:
        raise ResponsesRequestError("input must be a non-empty string or message array")
    for field in ("temperature", "top_p"):
        if field in payload:
            value = payload[field]
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ResponsesRequestError(f"{field} must be numeric")
            result[field] = value
    return result


def _usage(payload: dict[str, Any]) -> tuple[int, int] | None:
    usage = payload.get("usage")
    if not isinstance(usage, dict):
        return None
    prompt = usage.get("prompt_tokens")
    completion = usage.get("completion_tokens")
    if (
        type(prompt) is not int
        or prompt < 0
        or type(completion) is not int
        or completion < 0
    ):
        return None
    return prompt, completion


def _choice(payload: dict[str, Any]) -> tuple[str, str, str]:
    choices = payload.get("choices")
    if not isinstance(choices, list) or not choices or not isinstance(choices[0], dict):
        raise ValueError("upstream returned no chat completion choice")
    choice = choices[0]
    message = choice.get("message")
    if not isinstance(message, dict):
        raise ValueError("upstream returned an invalid chat completion")
    if message.get("tool_calls"):
        raise ValueError("upstream returned unsupported tool calls")
    content = message.get("content")
    if not isinstance(content, str):
        raise ValueError("upstream returned unsupported response content")
    return str(payload.get("id") or ""), content, str(choice.get("finish_reason") or "stop")


def chat_response_to_responses(payload: dict[str, Any], model: str) -> dict[str, Any]:
    upstream_id, text, finish_reason = _choice(payload)
    usage = _usage(payload)
    input_tokens, output_tokens = usage if usage is not None else (0, 0)
    incomplete = finish_reason == "length"
    response_id = f"resp_{uuid.uuid4().hex}"
    message_id = f"msg_{upstream_id.removeprefix('chatcmpl-') or uuid.uuid4().hex}"
    output = [
        {
            "id": message_id,
            "type": "message",
            "status": "completed",
            "role": "assistant",
            "content": [{"type": "output_text", "text": text, "annotations": []}],
        }
    ]
    return {
        "id": response_id,
        "object": "response",
        "created_at": int(time.time()),
        "status": "incomplete" if incomplete else "completed",
        "error": None,
        "incomplete_details": {"reason": "max_output_tokens"} if incomplete else None,
        "model": model,
        "output": output,
        "usage": {
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "total_tokens": input_tokens + output_tokens,
            "input_tokens_details": {"cached_tokens": 0},
            "output_tokens_details": {"reasoning_tokens": 0},
        },
    }


def _event(name: str, data: dict[str, Any]) -> bytes:
    payload = json.dumps(data, separators=(",", ":"), ensure_ascii=False)
    return f"event: {name}\ndata: {payload}\n\n".encode()


def _response_stub(response_id: str, model: str) -> dict[str, Any]:
    return {
        "id": response_id,
        "object": "response",
        "created_at": int(time.time()),
        "status": "in_progress",
        "error": None,
        "incomplete_details": None,
        "model": model,
        "output": [],
        "usage": None,
    }


def responses_sse(body: AsyncIterator[bytes], model: str) -> AsyncIterator[bytes]:
    async def converted() -> AsyncIterator[bytes]:
        response_id = f"resp_{uuid.uuid4().hex}"
        message_id = f"msg_{uuid.uuid4().hex}"
        response = _response_stub(response_id, model)
        yield _event("response.created", {"type": "response.created", "response": response})
        yield _event(
            "response.in_progress",
            {"type": "response.in_progress", "response": response},
        )
        yield _event(
            "response.output_item.added",
            {
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {
                    "id": message_id,
                    "type": "message",
                    "status": "in_progress",
                    "role": "assistant",
                    "content": [],
                },
            },
        )
        yield _event(
            "response.content_part.added",
            {
                "type": "response.content_part.added",
                "item_id": message_id,
                "output_index": 0,
                "content_index": 0,
                "part": {"type": "output_text", "text": "", "annotations": []},
            },
        )
        buffer = b""
        output_parts: list[str] = []
        input_tokens = output_tokens = 0
        finish_reason = "stop"
        failed = False
        try:
            async for chunk in body:
                buffer += chunk
                while True:
                    boundaries = [
                        (index, separator)
                        for separator in (b"\r\n\r\n", b"\n\n")
                        if (index := buffer.find(separator)) >= 0
                    ]
                    if not boundaries:
                        break
                    index, separator = min(boundaries, key=lambda item: item[0])
                    frame, buffer = buffer[:index], buffer[index + len(separator):]
                    lines = frame.replace(b"\r\n", b"\n").split(b"\n")
                    data = b"\n".join(
                        line[5:].lstrip(b" ") for line in lines if line.startswith(b"data:")
                    )
                    if not data or data.strip() == b"[DONE]":
                        continue
                    if failed:
                        continue
                    try:
                        event = json.loads(data)
                    except (TypeError, ValueError, UnicodeDecodeError):
                        continue
                    if not isinstance(event, dict):
                        continue
                    error = event.get("error")
                    if isinstance(error, dict):
                        failed = True
                        yield _event(
                            "response.failed",
                            {
                                "type": "response.failed",
                                "response": {
                                    **response,
                                    "status": "failed",
                                    "error": {
                                        "code": str(error.get("code") or "upstream_error"),
                                        "message": str(
                                            error.get("message") or "upstream request failed"
                                        ),
                                    },
                                },
                            },
                        )
                        break
                    usage = _usage(event)
                    if usage is not None:
                        input_tokens, output_tokens = usage
                    choices = event.get("choices")
                    if not isinstance(choices, list):
                        continue
                    for choice in choices:
                        if not isinstance(choice, dict):
                            continue
                        delta = choice.get("delta")
                        if isinstance(delta, dict) and delta.get("tool_calls"):
                            failed = True
                            yield _event(
                                "response.failed",
                                {
                                    "type": "response.failed",
                                    "response": {
                                        **response,
                                        "status": "failed",
                                        "error": {
                                            "code": "unsupported_tool_call",
                                            "message": "upstream returned unsupported tool calls",
                                        },
                                    },
                                },
                            )
                            break
                        text = delta.get("content") if isinstance(delta, dict) else None
                        if isinstance(text, str) and text:
                            output_parts.append(text)
                            yield _event(
                                "response.output_text.delta",
                                {
                                    "type": "response.output_text.delta",
                                    "item_id": message_id,
                                    "output_index": 0,
                                    "content_index": 0,
                                    "delta": text,
                                },
                            )
                        if choice.get("finish_reason") is not None:
                            finish_reason = str(choice["finish_reason"])
                    if failed:
                        break
                if len(buffer) > 1_048_576:
                    buffer = b""
            if not failed:
                text = "".join(output_parts)
                incomplete = finish_reason == "length"
                status = "incomplete" if incomplete else "completed"
                yield _event(
                    "response.output_text.done",
                    {
                        "type": "response.output_text.done",
                        "item_id": message_id,
                        "output_index": 0,
                        "content_index": 0,
                        "text": text,
                    },
                )
                yield _event(
                    "response.content_part.done",
                    {
                        "type": "response.content_part.done",
                        "item_id": message_id,
                        "output_index": 0,
                        "content_index": 0,
                        "part": {"type": "output_text", "text": text, "annotations": []},
                    },
                )
                item = {
                    "id": message_id,
                    "type": "message",
                    "status": "completed",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": text, "annotations": []}],
                }
                yield _event(
                    "response.output_item.done",
                    {
                        "type": "response.output_item.done",
                        "output_index": 0,
                        "item": item,
                    },
                )
                final = {
                    **response,
                    "status": status,
                    "incomplete_details": {"reason": "max_output_tokens"} if incomplete else None,
                    "output": [item],
                    "usage": {
                        "input_tokens": input_tokens,
                        "output_tokens": output_tokens,
                        "total_tokens": input_tokens + output_tokens,
                        "input_tokens_details": {"cached_tokens": 0},
                        "output_tokens_details": {"reasoning_tokens": 0},
                    },
                }
                yield _event(
                    "response.incomplete" if incomplete else "response.completed",
                    {
                        "type": "response.incomplete" if incomplete else "response.completed",
                        "response": final,
                    },
                )
        except Exception:
            yield _event(
                "response.failed",
                {
                    "type": "response.failed",
                    "response": {
                        **response,
                        "status": "failed",
                        "error": {
                            "code": "upstream_stream_interrupted",
                            "message": "upstream stream interrupted",
                        },
                    },
                },
            )
        finally:
            close = getattr(body, "aclose", None)
            if close is not None:
                await close()

    return converted()
