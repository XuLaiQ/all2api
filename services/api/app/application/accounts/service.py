from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from app.application.channels.service import ChannelService
from app.domain.channel import ProvisionFlowSpec


class ProvisionDispatchError(ValueError):
    """A requested operation cannot be dispatched to a registered provisioner."""


class ProvisionSchemaError(ProvisionDispatchError):
    """The flow payload does not satisfy its manifest schema."""


class ProvisionFlowNotFoundError(ProvisionSchemaError):
    """The requested flow is not declared by the selected channel."""


def _flow_map(flows: tuple[ProvisionFlowSpec, ...]) -> dict[str, ProvisionFlowSpec]:
    return {flow.id: flow for flow in flows}


def _validate_scalar(name: str, value: Any, schema: Mapping[str, Any]) -> None:
    expected = schema.get("type")
    if expected == "string" and not isinstance(value, str):
        raise ProvisionSchemaError(f"payload field {name} must be a string")
    if expected == "integer" and (isinstance(value, bool) or not isinstance(value, int)):
        raise ProvisionSchemaError(f"payload field {name} must be an integer")
    if expected == "boolean" and not isinstance(value, bool):
        raise ProvisionSchemaError(f"payload field {name} must be a boolean")
    if expected == "array" and not isinstance(value, list):
        raise ProvisionSchemaError(f"payload field {name} must be an array")
    if expected == "number" and (
        isinstance(value, bool) or not isinstance(value, (int, float))
    ):
        raise ProvisionSchemaError(f"payload field {name} must be a number")
    enum = schema.get("enum")
    if isinstance(enum, (list, tuple)) and value not in enum:
        raise ProvisionSchemaError(f"payload field {name} has an unsupported value")
    if isinstance(value, str):
        if isinstance(schema.get("minLength"), int) and len(value) < int(schema["minLength"]):
            raise ProvisionSchemaError(f"payload field {name} is shorter than minLength")
        if isinstance(schema.get("maxLength"), int) and len(value) > int(schema["maxLength"]):
            raise ProvisionSchemaError(f"payload field {name} exceeds maxLength")


def validate_flow_payload(flow: ProvisionFlowSpec, payload: Mapping[str, Any]) -> None:
    if not isinstance(payload, Mapping):
        raise ProvisionSchemaError("payload must be an object")
    schema = flow.schema if isinstance(flow.schema, Mapping) else {}
    properties = schema.get("properties", {})
    if not isinstance(properties, Mapping):
        properties = {}
    required = schema.get("required", ())
    for key in required if isinstance(required, (list, tuple)) else ():
        if key not in payload:
            raise ProvisionSchemaError(f"payload field {key} is required")
    if schema.get("additionalProperties") is False:
        unknown = sorted(set(payload) - set(properties))
        if unknown:
            raise ProvisionSchemaError(f"unknown payload field: {unknown[0]}")
    for key, value in payload.items():
        if key in properties and isinstance(properties[key], Mapping):
            _validate_scalar(str(key), value, properties[key])


class AccountProvisionService:
    """Generic channel → flow → provisioner dispatcher.

    The service knows only the port and manifest schema.  Platform-specific
    payload names and protocol details stay in the registered provisioner.
    """

    def __init__(self, registry: Mapping[str, Any]):
        self._channels = ChannelService(registry)

    def flow(self, channel: str, flow_id: str) -> ProvisionFlowSpec:
        schema = self._channels.provision_schema(channel)
        flows = tuple(
            ProvisionFlowSpec(
                id=item["id"],
                kind=item["kind"],
                schema=item.get("schema", {}),
                supports=item.get("supports", {}),
                timeout_seconds=item.get("timeout_seconds", 300),
                idempotency=item.get("idempotency", "required"),
                requires_admin=item.get("requires_admin", True),
            )
            for item in schema.get("flows", [])
        )
        try:
            return _flow_map(flows)[flow_id]
        except KeyError as exc:
            raise ProvisionFlowNotFoundError(
                f"flow {flow_id!r} is not registered for channel {channel!r}"
            ) from exc

    def validate(self, channel: str, flow_id: str, payload: Mapping[str, Any]) -> ProvisionFlowSpec:
        flow = self.flow(channel, flow_id)
        validate_flow_payload(flow, payload)
        return flow

    def provisioner(self, channel: str) -> Any:
        registration = self._channels.registration(channel)
        provisioner = getattr(registration, "provisioner", None)
        if provisioner is None:
            raise ProvisionDispatchError(f"channel {channel!r} has no account provisioner")
        return provisioner

    async def dispatch(
        self,
        channel: str,
        operation: str,
        *,
        flow: str | None = None,
        payload: Mapping[str, Any] | None = None,
        session_id: str = "",
        idempotency_key: str = "",
    ) -> Mapping[str, Any] | None:
        payload = payload or {}
        if flow is not None:
            self.validate(channel, flow, payload)
        provisioner = self.provisioner(channel)
        method = getattr(provisioner, operation, None)
        if not callable(method):
            raise ProvisionDispatchError(
                f"operation {operation!r} is not supported for channel {channel!r}"
            )
        if operation == "start":
            return await method(flow or "", payload, idempotency_key)
        if operation == "poll":
            return await method(session_id, idempotency_key)
        if operation == "complete":
            return await method(session_id, payload, idempotency_key)
        if operation == "import_accounts":
            return await method(payload, idempotency_key)
        if operation == "cancel":
            await method(session_id, idempotency_key)
            return None
        raise ProvisionDispatchError(f"unsupported provision operation {operation!r}")
