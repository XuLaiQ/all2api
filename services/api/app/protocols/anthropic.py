from __future__ import annotations

import json
import uuid
from collections.abc import AsyncIterator
from typing import Any


class AnthropicRequestError(ValueError):
    pass


def _text_blocks(value: Any, field: str) -> str:
    if isinstance(value, str):
        return value
    if not isinstance(value, list):
        raise AnthropicRequestError(f"{field} must be text or an array of text blocks")
    parts = []
    for block in value:
        if not isinstance(block, dict) or block.get("type") != "text":
            raise AnthropicRequestError(f"{field} only supports text content blocks")
        text = block.get("text")
        if not isinstance(text, str):
            raise AnthropicRequestError(f"{field} text blocks require a text value")
        parts.append(text)
    return "".join(parts)


def _message_content(role: str, value: Any, field: str) -> tuple[str, list[dict[str, Any]]]:
    if isinstance(value, str):
        return value, []
    if not isinstance(value, list):
        raise AnthropicRequestError(f"{field} must be text or an array of content blocks")
    text_parts = []
    tool_calls = []
    for index, block in enumerate(value):
        block_field = f"{field}[{index}]"
        if not isinstance(block, dict):
            raise AnthropicRequestError(f"{block_field} must be an object")
        block_type = block.get("type")
        if block_type == "text":
            text = block.get("text")
            if not isinstance(text, str):
                raise AnthropicRequestError(f"{block_field}.text must be a string")
            text_parts.append(text)
        elif block_type == "tool_use" and role == "assistant":
            name = block.get("name")
            tool_id = block.get("id")
            if not isinstance(name, str) or not name or not isinstance(tool_id, str) or not tool_id:
                raise AnthropicRequestError(f"{block_field} requires a tool name and id")
            input_value = block.get("input", {})
            if not isinstance(input_value, dict):
                raise AnthropicRequestError(f"{block_field}.input must be an object")
            tool_calls.append(
                {
                    "id": tool_id,
                    "type": "function",
                    "function": {
                        "name": name,
                        "arguments": json.dumps(input_value, separators=(",", ":")),
                    },
                }
            )
        elif block_type == "tool_result" and role == "user":
            tool_id = block.get("tool_use_id")
            if not isinstance(tool_id, str) or not tool_id:
                raise AnthropicRequestError(f"{block_field}.tool_use_id is required")
            if type(block.get("is_error", False)) is not bool:
                raise AnthropicRequestError(f"{block_field}.is_error must be a boolean")
            if block.get("is_error", False):
                raise AnthropicRequestError(f"{block_field}.is_error is not supported")
            tool_content = _text_blocks(block.get("content", ""), f"{block_field}.content")
            tool_calls.append(
                {"role": "tool", "tool_call_id": tool_id, "content": tool_content}
            )
        else:
            raise AnthropicRequestError(f"{block_field} has an unsupported type or role")
    return "".join(text_parts), tool_calls


def normalize_messages_request(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise AnthropicRequestError("request body must be a JSON object")
    allowed = {
        "model",
        "messages",
        "max_tokens",
        "system",
        "stream",
        "temperature",
        "top_p",
        "stop_sequences",
        "metadata",
        "tools",
        "tool_choice",
    }
    unsupported = sorted(set(payload) - allowed)
    if unsupported:
        raise AnthropicRequestError(
            f"unsupported request field: {unsupported[0]}"
        )
    model = payload.get("model")
    if not isinstance(model, str) or not model.strip():
        raise AnthropicRequestError("model is required")
    max_tokens = payload.get("max_tokens")
    if type(max_tokens) is not int or max_tokens < 1:
        raise AnthropicRequestError("max_tokens must be a positive integer")
    stream = payload.get("stream", False)
    if not isinstance(stream, bool):
        raise AnthropicRequestError("stream must be a boolean")
    source_messages = payload.get("messages")
    if not isinstance(source_messages, list) or not source_messages:
        raise AnthropicRequestError("messages must be a non-empty array")

    messages: list[dict[str, Any]] = []
    seen_tool_use_ids: set[str] = set()
    pending_tool_results: set[str] = set()
    if "system" in payload:
        messages.append({"role": "system", "content": _text_blocks(payload["system"], "system")})
    for index, message in enumerate(source_messages):
        if not isinstance(message, dict) or message.get("role") not in {"user", "assistant"}:
            raise AnthropicRequestError(f"messages[{index}].role must be user or assistant")
        role = message["role"]
        content, tool_items = _message_content(
            role,
            message.get("content"),
            f"messages[{index}].content",
        )
        if role == "assistant":
            assistant_message: dict[str, Any] = {"role": role, "content": content}
            if tool_items:
                for item in tool_items:
                    tool_id = item["id"]
                    if tool_id in seen_tool_use_ids:
                        raise AnthropicRequestError("tool_use ids must be unique")
                    seen_tool_use_ids.add(tool_id)
                    pending_tool_results.add(tool_id)
                assistant_message["tool_calls"] = tool_items
            messages.append(assistant_message)
        else:
            if content:
                messages.append({"role": role, "content": content})
            for item in tool_items:
                tool_id = item["tool_call_id"]
                if tool_id not in pending_tool_results:
                    raise AnthropicRequestError("tool_result must reference a previous tool_use")
                pending_tool_results.remove(tool_id)
            messages.extend(tool_items)

    result: dict[str, Any] = {
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
        "stream": stream,
    }
    for field in ("temperature", "top_p"):
        if field in payload:
            value = payload[field]
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise AnthropicRequestError(f"{field} must be numeric")
            result[field] = value
    if "stop_sequences" in payload:
        stop = payload["stop_sequences"]
        if not isinstance(stop, list) or any(not isinstance(item, str) for item in stop):
            raise AnthropicRequestError("stop_sequences must be an array of strings")
        result["stop"] = stop
    tools = payload.get("tools")
    if tools is not None:
        if not isinstance(tools, list):
            raise AnthropicRequestError("tools must be an array")
        functions = []
        seen_names = set()
        for index, tool in enumerate(tools):
            field = f"tools[{index}]"
            if not isinstance(tool, dict):
                raise AnthropicRequestError(f"{field} must be an object")
            if set(tool) - {"name", "description", "input_schema"}:
                raise AnthropicRequestError(f"{field} contains an unsupported field")
            name = tool.get("name")
            schema = tool.get("input_schema")
            if not isinstance(name, str) or not name or name in seen_names:
                raise AnthropicRequestError(f"{field}.name must be unique and non-empty")
            if not isinstance(schema, dict) or schema.get("type") != "object":
                raise AnthropicRequestError(f"{field}.input_schema must describe an object")
            seen_names.add(name)
            function: dict[str, Any] = {
                "name": name,
                "parameters": schema,
            }
            description = tool.get("description")
            if description is not None:
                if not isinstance(description, str):
                    raise AnthropicRequestError(f"{field}.description must be a string")
                function["description"] = description
            functions.append({"type": "function", "function": function})
        result["tools"] = functions
    tool_choice = payload.get("tool_choice")
    if tool_choice is not None:
        if not isinstance(tool_choice, dict):
            raise AnthropicRequestError("tool_choice must be an object")
        choice_type = tool_choice.get("type")
        if choice_type == "auto":
            result["tool_choice"] = "auto"
        elif choice_type == "any":
            result["tool_choice"] = "required"
        elif choice_type == "none":
            result["tool_choice"] = "none"
        elif choice_type == "tool":
            name = tool_choice.get("name")
            if not isinstance(name, str) or not name:
                raise AnthropicRequestError("tool_choice.name is required")
            result["tool_choice"] = {"type": "function", "function": {"name": name}}
        else:
            raise AnthropicRequestError("tool_choice.type must be auto, any, none, or tool")
        if choice_type == "tool" and "tools" in payload:
            if name not in seen_names:
                raise AnthropicRequestError("tool_choice.name must match a declared tool")
        disable_parallel = tool_choice.get("disable_parallel_tool_use", False)
        if type(disable_parallel) is not bool:
            raise AnthropicRequestError("disable_parallel_tool_use must be a boolean")
        if disable_parallel:
            result["parallel_tool_calls"] = False
    metadata = payload.get("metadata")
    if metadata is not None:
        if not isinstance(metadata, dict):
            raise AnthropicRequestError("metadata must be an object")
        user_id = metadata.get("user_id")
        if user_id is not None:
            if not isinstance(user_id, str):
                raise AnthropicRequestError("metadata.user_id must be a string")
            result["user"] = user_id
    return result


def anthropic_error_type(status: int, code: Any = None) -> str:
    code = str(code or "").lower()
    if status == 401 or code in {"invalid_api_key", "authentication_error"}:
        return "authentication_error"
    if status == 403:
        return "permission_error"
    if status == 404:
        return "not_found_error"
    if status == 429:
        return "rate_limit_error"
    if status == 529:
        return "overloaded_error"
    if status in {400, 422}:
        return "invalid_request_error"
    return "api_error"


def anthropic_error_payload(message: str, status: int, code: Any = None) -> dict[str, Any]:
    return {
        "type": "error",
        "error": {"type": anthropic_error_type(status, code), "message": message},
    }


def openai_error_to_anthropic(body: bytes, status: int) -> dict[str, Any]:
    try:
        payload = json.loads(body)
    except (TypeError, ValueError, UnicodeDecodeError):
        payload = None
    error = payload.get("error") if isinstance(payload, dict) else None
    message = error.get("message") if isinstance(error, dict) else None
    code = error.get("code") if isinstance(error, dict) else None
    if not isinstance(message, str):
        message = "upstream request failed"
    return anthropic_error_payload(message, status, code)


def _stop_reason(reason: Any) -> str:
    if reason == "length":
        return "max_tokens"
    if reason == "tool_calls":
        return "tool_use"
    return "end_turn"


def _reported_usage(usage: Any) -> tuple[int, int] | None:
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


def chat_response_to_messages(payload: dict[str, Any], model: str) -> dict[str, Any]:
    choices = payload.get("choices")
    if not isinstance(choices, list) or not choices or not isinstance(choices[0], dict):
        raise ValueError("upstream returned no chat completion choice")
    choice = choices[0]
    message = choice.get("message")
    if not isinstance(message, dict):
        raise ValueError("upstream returned an invalid chat completion")
    content = message.get("content")
    if content is not None and not isinstance(content, str):
        raise ValueError("upstream returned unsupported message content")
    usage = _reported_usage(payload.get("usage"))
    input_tokens, output_tokens = usage if usage is not None else (0, 0)
    blocks = [{"type": "text", "text": content}] if content else []
    tool_calls = message.get("tool_calls") or []
    if not isinstance(tool_calls, list):
        raise ValueError("upstream returned invalid tool calls")
    seen_ids = set()
    for call in tool_calls:
        if not isinstance(call, dict) or not isinstance(call.get("function"), dict):
            raise ValueError("upstream returned an invalid tool call")
        function = call["function"]
        name = function.get("name")
        raw_arguments = function.get("arguments", "{}")
        if not isinstance(name, str) or not name or not isinstance(raw_arguments, str):
            raise ValueError("upstream returned an invalid tool call")
        call_id = call.get("id")
        if not isinstance(call_id, str) or not call_id or call_id in seen_ids:
            raise ValueError("upstream returned an invalid tool call id")
        seen_ids.add(call_id)
        try:
            arguments = json.loads(raw_arguments)
        except (TypeError, ValueError):
            raise ValueError("upstream returned malformed tool arguments") from None
        if not isinstance(arguments, dict):
            raise ValueError("upstream returned non-object tool arguments")
        blocks.append(
            {
                "type": "tool_use",
                "id": call_id,
                "name": name,
                "input": arguments,
            }
        )
    if choice.get("finish_reason") == "tool_calls" and not tool_calls:
        raise ValueError("upstream reported tool calls without any calls")
    if tool_calls:
        choice["finish_reason"] = "tool_calls"
    return {
        "id": f"msg_{uuid.uuid4().hex}",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": blocks,
        "stop_reason": _stop_reason(choice.get("finish_reason")),
        "stop_sequence": None,
        "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens},
    }


def _event(name: str, data: dict[str, Any]) -> bytes:
    encoded = json.dumps(data, separators=(",", ":"), ensure_ascii=False)
    return f"event: {name}\ndata: {encoded}\n\n".encode()


def _pop_sse_event(buffer: bytes) -> tuple[bytes, Any | None]:
    boundaries = [
        (index, separator)
        for separator in (b"\r\n\r\n", b"\n\n")
        if (index := buffer.find(separator)) >= 0
    ]
    if not boundaries:
        return buffer, None
    index, separator = min(boundaries, key=lambda item: item[0])
    frame, buffer = buffer[:index], buffer[index + len(separator):]
    data_lines = []
    for line in frame.replace(b"\r\n", b"\n").split(b"\n"):
        if line.startswith(b"data:"):
            value = line[5:]
            data_lines.append(value[1:] if value.startswith(b" ") else value)
    if not data_lines:
        return buffer, None
    data = b"\n".join(data_lines)
    if data.strip() == b"[DONE]":
        return buffer, "done"
    try:
        return buffer, json.loads(data)
    except (TypeError, ValueError, UnicodeDecodeError):
        return buffer, None


def messages_sse(
    body: AsyncIterator[bytes],
    model: str,
    *,
    allow_parallel_tools: bool = True,
) -> AsyncIterator[bytes]:
    async def converted() -> AsyncIterator[bytes]:
        message_id = f"msg_{uuid.uuid4().hex}"
        yield _event(
            "message_start",
            {
                "type": "message_start",
                "message": {
                    "id": message_id,
                    "type": "message",
                    "role": "assistant",
                    "model": model,
                    "content": [],
                    "stop_reason": None,
                    "stop_sequence": None,
                    "usage": {"input_tokens": 0, "output_tokens": 0},
                },
            },
        )
        buffer = b""
        output_tokens = 0
        stop_reason = "end_turn"
        failed = False
        next_block_index = 0
        text_block_index: int | None = None
        open_blocks: set[int] = set()
        tool_call_states: dict[int, dict[str, Any]] = {}
        seen_tool_ids: set[str] = set()
        try:
            async for chunk in body:
                buffer += chunk
                while True:
                    buffer, event = _pop_sse_event(buffer)
                    if event is None:
                        break
                    if event == "done":
                        continue
                    if failed:
                        continue
                    if not isinstance(event, dict):
                        continue
                    error = event.get("error")
                    if isinstance(error, dict):
                        for block_index in sorted(open_blocks):
                            yield _event(
                                "content_block_stop",
                                {"type": "content_block_stop", "index": block_index},
                            )
                        open_blocks.clear()
                        message = error.get("message")
                        yield _event(
                            "error",
                            anthropic_error_payload(
                                message if isinstance(message, str) else "upstream request failed",
                                502,
                                error.get("code"),
                            ),
                        )
                        yield _event("message_stop", {"type": "message_stop"})
                        failed = True
                        break
                    usage = _reported_usage(event.get("usage"))
                    if usage is not None:
                        _, output_tokens = usage
                    choices = event.get("choices")
                    if not isinstance(choices, list):
                        continue
                    for choice in choices:
                        if not isinstance(choice, dict):
                            continue
                        delta = choice.get("delta")
                        tool_deltas = delta.get("tool_calls") if isinstance(delta, dict) else None
                        text = delta.get("content") if isinstance(delta, dict) else None
                        if isinstance(text, str) and text:
                            if text_block_index is None:
                                text_block_index = next_block_index
                                next_block_index += 1
                                open_blocks.add(text_block_index)
                                yield _event(
                                    "content_block_start",
                                    {
                                        "type": "content_block_start",
                                        "index": text_block_index,
                                        "content_block": {"type": "text", "text": ""},
                                    },
                                )
                            yield _event(
                                "content_block_delta",
                                {
                                    "type": "content_block_delta",
                                    "index": text_block_index,
                                    "delta": {"type": "text_delta", "text": text},
                                },
                            )
                        if tool_deltas:
                            if text_block_index is not None:
                                open_blocks.remove(text_block_index)
                                yield _event(
                                    "content_block_stop",
                                    {"type": "content_block_stop", "index": text_block_index},
                                )
                                text_block_index = None
                            for tool_delta in tool_deltas:
                                if (
                                    not isinstance(tool_delta, dict)
                                    or type(tool_delta.get("index")) is not int
                                    or tool_delta["index"] < 0
                                ):
                                    raise ValueError("upstream returned an invalid tool call index")
                                tool_index = tool_delta["index"]
                                if not allow_parallel_tools and tool_index != 0:
                                    raise ValueError(
                                        "upstream returned parallel calls when disabled"
                                    )
                                state = tool_call_states.get(tool_index)
                                if state is None:
                                    state = {
                                        "block_index": next_block_index,
                                        "id": None,
                                        "name": "",
                                        "arguments": "",
                                        "emitted_arguments": 0,
                                        "started": False,
                                    }
                                    tool_call_states[tool_index] = state
                                    next_block_index += 1
                                if tool_delta.get("id"):
                                    if state["id"] is not None and state["id"] != tool_delta["id"]:
                                        raise ValueError("upstream changed a tool call id")
                                    state["id"] = tool_delta["id"]
                                function = tool_delta.get("function")
                                if function is not None and not isinstance(function, dict):
                                    raise ValueError("upstream returned an invalid tool call")
                                if isinstance(function, dict):
                                    name = function.get("name")
                                    if isinstance(name, str):
                                        state["name"] += name
                                    arguments = function.get("arguments")
                                    if isinstance(arguments, str):
                                        state["arguments"] += arguments
                                if (
                                    state["id"]
                                    and state["name"]
                                    and state["arguments"]
                                    and not state["started"]
                                ):
                                    if state["id"] in seen_tool_ids:
                                        raise ValueError(
                                            "upstream returned duplicate tool call ids"
                                        )
                                    seen_tool_ids.add(state["id"])
                                    state["started"] = True
                                    open_blocks.add(state["block_index"])
                                    yield _event(
                                        "content_block_start",
                                        {
                                            "type": "content_block_start",
                                            "index": state["block_index"],
                                            "content_block": {
                                                "type": "tool_use",
                                                "id": state["id"],
                                                "name": state["name"],
                                                "input": {},
                                            },
                                        },
                                    )
                                if state["started"]:
                                    new_arguments = state["arguments"][state["emitted_arguments"]:]
                                    if new_arguments:
                                        yield _event(
                                            "content_block_delta",
                                            {
                                                "type": "content_block_delta",
                                                "index": state["block_index"],
                                                "delta": {
                                                    "type": "input_json_delta",
                                                    "partial_json": new_arguments,
                                                },
                                            },
                                        )
                                        state["emitted_arguments"] = len(state["arguments"])
                        reason = choice.get("finish_reason")
                        if reason is not None:
                            stop_reason = _stop_reason(reason)
                if failed:
                    continue
                if len(buffer) > 1_048_576:
                    buffer = b""
            if not failed:
                for state in tool_call_states.values():
                    if not state["started"]:
                        raise ValueError("upstream returned an incomplete tool call")
                    try:
                        arguments = json.loads(state["arguments"] or "{}")
                    except (TypeError, ValueError):
                        raise ValueError("upstream returned malformed tool arguments") from None
                    if not isinstance(arguments, dict):
                        raise ValueError("upstream returned non-object tool arguments")
                for block_index in sorted(open_blocks):
                    yield _event(
                        "content_block_stop",
                        {"type": "content_block_stop", "index": block_index},
                    )
                open_blocks.clear()
                if tool_call_states:
                    stop_reason = "tool_use"
                yield _event(
                    "message_delta",
                    {
                        "type": "message_delta",
                        "delta": {"stop_reason": stop_reason, "stop_sequence": None},
                        "usage": {"output_tokens": output_tokens},
                    },
                )
                yield _event("message_stop", {"type": "message_stop"})
        except Exception:
            for block_index in sorted(open_blocks):
                yield _event(
                    "content_block_stop",
                    {"type": "content_block_stop", "index": block_index},
                )
            open_blocks.clear()
            yield _event(
                "error",
                anthropic_error_payload("upstream stream interrupted", 502),
            )
            yield _event("message_stop", {"type": "message_stop"})
        finally:
            close = getattr(body, "aclose", None)
            if close is not None:
                await close()

    return converted()
