from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
import json
import logging
import re
from threading import Event
from typing import Any, Callable, Protocol
from uuid import uuid4


VERSION = 3
MAX_PAYLOAD = 1 << 20
IDENTIFIER = re.compile(r"[a-zA-Z0-9_.:-]{1,128}")
CBOR_MARKER = b"\xd9\xd9\xf7"
Message = dict[str, Any]
log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Delivery:
    topic: str = ""
    data: bytes = b""
    reset: bool = False
    retained: bool = False


class Transport(Protocol):
    def publish(self, topic: str, data: bytes) -> None: ...
    def receive(self, timeout: float) -> Delivery | None: ...
    def close(self) -> None: ...


def message(kind: str, source: str, data: Any, *, observed_at: str | None = None, **fields: Any) -> Message:
    return {
        **fields,
        "version": VERSION,
        "id": uuid4().hex,
        "type": kind,
        "source": source,
        "observed_at": observed_at or datetime.now(timezone.utc).isoformat(),
        "data": data,
    }


def observation_message(source, kind, observations, *, schema_version="1", **fields):
    fields.setdefault("expires_at", (datetime.now(timezone.utc) + timedelta(minutes=1)).isoformat())
    return message("observation.submitted", source,
                   {"event_type": kind, "observations": observations},
                   schema_version=schema_version, **fields)


def route(value: Message) -> str:
    fields = [value["type"], value["source"], value.get("target") or "_"]
    if not all(isinstance(part, str) and IDENTIFIER.fullmatch(part) for part in fields):
        raise ValueError("invalid message route")
    return "/".join(["msg", *fields])


def encode(value: Message, encoding: str = "json") -> bytes:
    if encoding not in {"json", "cbor"}:
        raise ValueError(f"unknown encoding {encoding!r}")
    json_data = json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":"))
    if encoding == "json":
        payload = json_data.encode()
    else:
        import cbor2

        payload = CBOR_MARKER + cbor2.dumps(json.loads(json_data))
    if len(payload) > MAX_PAYLOAD:
        raise ValueError("message exceeds maximum payload")
    return payload


def decode(data: bytes) -> Message:
    if len(data) > MAX_PAYLOAD:
        raise ValueError("message exceeds maximum payload")
    if data.startswith(CBOR_MARKER):
        from io import BytesIO
        import cbor2

        stream = BytesIO(data[len(CBOR_MARKER):])
        value = cbor2.load(stream)
        if stream.read(1):
            raise ValueError("trailing CBOR data")
    else:
        value = json.loads(data)
    if not isinstance(value, dict) or value.get("version") != VERSION:
        raise ValueError("unsupported message envelope")
    return value


def caused_by(parent: Message) -> dict[str, str]:
    return {
        "causation_id": parent["id"],
        "correlation_id": parent.get("correlation_id") or parent["id"],
    }


class Client:
    def __init__(self, transport: Transport, source: str, service_type: str, encoding: str = "json"):
        self.transport = transport
        self.source = source
        self.service_type = service_type
        self.encoding = encoding

    def send(self, value: Message) -> None:
        if value["source"] != self.source:
            raise ValueError("message source does not match client")
        self.transport.publish(route(value), encode(value, self.encoding))

    def register(
        self,
        name: str,
        *,
        devices: list[dict[str, Any]],
        events: dict[str, Any] | None = None,
        actions: dict[str, Any] | None = None,
        idempotent_actions: list[str] | None = None,
    ) -> None:
        self.send(message("service.registered", self.source, {
            "uuid": self.source,
            "name": name,
            "service_type": self.service_type,
            "devices": devices,
            "events": events or {},
            "actions": actions or {},
            "idempotent_actions": idempotent_actions or [],
        }))

    def event(self, kind: str, data: Any, *, device: str, values: dict[str, Any],
              schema_version: str = "1") -> None:
        self.observe_batch(kind, [{"device_id": device, "values": values, "data": data}],
                           schema_version=schema_version)

    def observe_batch(self, kind, observations, *, schema_version="1", cause=None):
        meta = caused_by(cause) if cause else {}
        if cause and cause.get("action_id"):
            meta["action_id"] = cause["action_id"]
        self.send(observation_message(self.source, kind, observations,
                                      schema_version=schema_version, **meta))

    def report(self, device: str, values: dict[str, Any], *, cause: Message | None = None) -> None:
        self.observe_batch("device.observed", [{"device_id": device, "values": values}], cause=cause)

    def outcome(self, request: Message, status: str, error: str = "", **result: Any) -> None:
        if status not in {"accepted", "submitted", "completed", "failed"}:
            raise ValueError(f"invalid action outcome {status!r}")
        self.send(message(
            "action.outcome", self.source, {"status": status, "error": error, **result},
            target=request["source"], device_id=request["device_id"],
            action_id=request["action_id"], **caused_by(request),
        ))

    def serve(
        self,
        stop: Event,
        registration: dict[str, Any],
        handler: Callable[[Message, str, dict[str, Any]], None],
    ) -> None:
        """Register capabilities and handle actions serially until stop is set."""
        self.register(**registration)
        recent: OrderedDict[tuple[str, str], tuple[str, str]] = OrderedDict()
        devices = {device["id"]: device for device in registration["devices"]}
        schemas = registration.get("actions", {})
        while not stop.is_set():
            delivery = self.transport.receive(0.3)
            if delivery is None:
                continue
            if delivery.reset:
                self.register(**registration)
                continue
            if delivery.retained:
                continue
            try:
                request = decode(delivery.data)
                if request["type"] != "action.requested" or request.get("target") != self.source:
                    continue
                if delivery.topic != route(request):
                    continue
                key, action, device = (request["source"], request["action_id"]), request["data"], request["device_id"]
                expires = request.get("expires_at")
                if expires and datetime.now(timezone.utc) >= datetime.fromisoformat(expires.replace("Z", "+00:00")):
                    continue
                name, args = action["name"], action["args"]
                if not isinstance(args, dict) or not isinstance(name, str):
                    continue
                if name not in devices.get(device, {}).get("actions", ()):
                    continue
                if request.get("schema_version") not in schemas.get(name, {}):
                    continue
            except (KeyError, ValueError, TypeError, OverflowError):
                log.warning("Discarding malformed action request")
                continue
            if key in recent:
                self.outcome(request, *recent[key])
                continue
            self.outcome(request, "accepted")
            try:
                handler(request, name, args)
            except Exception as exc:
                log.exception("Action %s failed", request["action_id"])
                result = ("failed", str(exc))
            else:
                result = ("completed", "")
            recent[key] = result
            if len(recent) > 4096:
                recent.popitem(last=False)
            self.outcome(request, *result)
