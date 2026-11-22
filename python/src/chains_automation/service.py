from collections import OrderedDict, deque
from contextvars import ContextVar
from copy import deepcopy
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from functools import wraps
from inspect import getattr_static, isfunction
import json
import logging
import os
from pathlib import Path
from queue import Empty, Full, Queue
import re
import signal
import unicodedata
from tempfile import NamedTemporaryFile
from threading import Event, Lock, Thread, current_thread, get_ident, main_thread
from time import monotonic
from urllib.parse import urlsplit
from uuid import uuid4

from jsonschema import Draft202012Validator

from .client import Client, IDENTIFIER, MAX_PAYLOAD, caused_by, decode, encode, message, observation_message, route
from .description import Capability
from .service_config import ServiceConfig, stable_identity
from .transports import MQTT, RedisStreams


log = logging.getLogger(__name__)
DEFAULT_DEVICE = "system"
CONTROL_TYPES = ("action.requested", "state.query.responded")
INTERNAL_TYPES = {
    "action.requested", "state.reported", "observation.submitted", "device.observed", "state.committed", "state.head",
    "state.query.requested", "state.query.responded", "state.snapshot.requested",
    "state.snapshot.responded", "service.registered", "service.heartbeat",
}
STATE_SCHEMA = {
    "type": "object", "additionalProperties": False,
    "properties": {"device_id": {"type": "string"}, "attribute": {"type": "string"}},
    "dependentRequired": {"attribute": ["device_id"]},
}
_event = ContextVar("chains_event", default=None)
_action = ContextVar("chains_action", default=None)


def utcnow():
    return datetime.now(timezone.utc)


def event_method(method):
    @wraps(method)
    def emit(self, *args, **kwargs):
        cap = self._events[method.__name__]
        bound = cap.signature.bind(*args, **kwargs)
        bound.apply_defaults()
        token = _event.set((self, method.__name__, bound.arguments))
        try:
            return method(self, *args, **kwargs)
        finally:
            _event.reset(token)
    return emit


class Service:
    """Base class for a Chains integration. Own hardware in lifecycle hooks."""

    service_type = None
    settings_type = None
    attributes = ()
    description = ""
    event_attributes = {}
    retain_devices = False

    def __init_subclass__(cls, **kwargs):
        super().__init_subclass__(**kwargs)
        for name, method in vars(cls).copy().items():
            if name.startswith("event_") and callable(method):
                setattr(cls, name, event_method(method))

    def __init__(self, uri=None, *, config_path=None, transport=None, settings=None, **options):
        inferred = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", "_", type(self).__name__).lower()
        self.service_type = self.service_type or inferred
        if not isinstance(self.service_type, str) or not IDENTIFIER.fullmatch(self.service_type) or self.service_type in {".", ".."}:
            raise ValueError("Invalid service type")
        self.config, self.settings = ServiceConfig.load(
            self.service_type, config_path, uri=uri, settings_type=self.settings_type,
            settings=settings, **options,
        )
        self.transport_factory = self.open_transport
        self._subscriptions = set()
        self._injected_transport = transport
        self._client = None
        self._registered = False
        self._started = False
        self._closed = False
        self._stop = Event()
        self._wake = Event()
        self._send_lock = Lock()
        self._state_lock = Lock()
        self._query_lock = Lock()
        self._state = {}
        self._pending = deque()
        self._queries = {}
        self._outgoing_queries = Queue(128)
        self._actions_queue = Queue(128)
        self._hooks_queue = Queue(256)
        self._threads = []
        self._resync = Event()
        self._instance_id = uuid4().hex

    def onInit(self):
        """Configure subscriptions, set up hardware, and optionally seed state."""

    def onStart(self):
        """Called after registration and after background workers start."""

    def onPoll(self):
        """Perform one hardware poll. The SDK waits between calls."""

    def onEvent(self, event):
        """Called for subscribed public events."""

    def onReconnect(self):
        """Called after re-registration and silent state synchronization."""

    def onShutdown(self):
        """Clean up before the transport closes."""

    def subscribe(self, *types):
        if self._client is not None:
            raise RuntimeError("Set subscriptions in onInit() before connecting")
        if any(kind != "*" and not IDENTIFIER.fullmatch(kind) for kind in types):
            raise ValueError("Invalid event subscription")
        self._subscriptions.update(types)

    @staticmethod
    def _attribute_descriptions(attributes):
        if not isinstance(attributes, dict):
            attributes = {name: {"schema": {}} for name in attributes}
        for name, descriptor in attributes.items():
            if not IDENTIFIER.fullmatch(name) or name == "_attributes":
                raise ValueError(f"Invalid attribute {name}")
            Draft202012Validator.check_schema(descriptor["schema"])
        return attributes

    @staticmethod
    def _normalize_placement(device):
        for key in ("location", "tags"):
            if key not in device:
                continue
            parts = device[key]
            if not isinstance(parts, (list, tuple)) or len(parts) > 32 or any(
                not isinstance(part, str) or not part or len(part.encode("utf-8")) > 128
                or part.strip() != part or part in {"*", "**"}
                or any(unicodedata.category(char) == "Cc" for char in part)
                for part in parts
            ):
                raise ValueError(f"Invalid device {key}")
            device[key] = sorted(set(parts)) if key == "tags" else list(parts)

    def describe_devices(self):
        return [{"id": DEFAULT_DEVICE, "name": "System", "classes": [],
                 "attributes": self._attribute_descriptions(deepcopy(self.attributes))}]

    def describe_action_schema(self, name, version, schema):
        """Return the configured action schema, or None to omit the action."""
        return schema

    def describe(self):
        """Reflect the service registration without invoking methods or connecting."""
        caps = {"event": {}, "action": {}}
        schemas = {"event": {}, "action": {}}
        devices = deepcopy(self.describe_devices())
        if not devices:
            raise ValueError("Declare at least one device")
        ids = [d["id"] for d in devices]
        if len(ids) != len(set(ids)) or any(not IDENTIFIER.fullmatch(value) for value in ids):
            raise ValueError("Invalid or duplicate device ID")
        idempotent = set()
        for method_name in dir(type(self)):
            kind, _, suffix = method_name.partition("_")
            if kind not in caps or not suffix or not isfunction(getattr_static(type(self), method_name)):
                continue
            cap = Capability.discover(suffix, getattr(self, method_name))
            if cap.device is not None and cap.device not in ids:
                raise ValueError(f"{method_name} refers to an unknown device")
            key = (cap.name, cap.version)
            if key in schemas[kind] or (kind == "action" and cap.name == "get_state"):
                raise ValueError(f"Duplicate or reserved capability {cap.name}")
            if kind == "event" and (cap.name in INTERNAL_TYPES or cap.name in {
                "state.changed", "service.availability.changed", "action.outcome"
            }):
                raise ValueError(f"Reserved event name {cap.name}")
            if kind == "action":
                schema = self.describe_action_schema(cap.name, cap.version, cap.validator.schema)
                if schema is None:
                    continue
                Draft202012Validator.check_schema(schema)
                cap = replace(cap, validator=Draft202012Validator(schema))
            schemas[kind][key] = cap.validator.schema
            caps[kind][method_name] = cap
            if kind == "action" and cap.idempotent:
                idempotent.add(cap.name)
        for cap in caps["action"].values():
            if cap.name in idempotent and not cap.idempotent:
                raise ValueError(f"All versions of {cap.name} must agree on idempotence")
        self._default_device = DEFAULT_DEVICE if DEFAULT_DEVICE in ids else ids[0]
        for device in devices:
            self._normalize_placement(device)
            attributes = self._attribute_descriptions(device.get("attributes", {}))
            device.setdefault("classes", [])
            for method, cap in caps["event"].items():
                attribute = self._event_attribute(method)
                if attribute and (cap.device or self._default_device) == device["id"]:
                    attributes.setdefault(attribute, {"schema": cap.validator.schema})
            device["attributes"] = attributes
            actions = {cap.name for cap in caps["action"].values()
                       if (cap.device or self._default_device) == device["id"]}
            device["actions"] = sorted(actions | {"get_state"})
        self._events = caps["event"]
        self._handlers = {(cap.name, cap.version): (cap, getattr(self, method_name))
                          for method_name, cap in caps["action"].items()}
        schemas["action"]["get_state", "1"] = STATE_SCHEMA
        idempotent.add("get_state")

        def versions(entries):
            result = {}
            for (name, version), schema in entries.items():
                result.setdefault(name, {})[version] = schema
            return result

        if self.config.service_id == "":
            self.config.service_id = stable_identity(self.service_type, self.config.instance)
        return {"uuid": self.config.service_id, "service_type": self.service_type,
                "name": self.config.name or self.config.instance, "description": self.description, "devices": devices,
                "retain_devices": self.retain_devices,
                "events": versions(schemas["event"]), "actions": versions(schemas["action"]),
                "idempotent_actions": sorted(idempotent)}

    @staticmethod
    def open_transport(config, types):
        try:
            scheme = urlsplit(config.uri).scheme
        except (ValueError, TypeError):
            raise ValueError("Invalid Chains bus URI") from None
        if scheme not in {"mqtt", "mqtts", "mqtt+unix", "redis", "rediss"}:
            raise ValueError("Unsupported Chains bus URI scheme")
        transport = RedisStreams if scheme in {"redis", "rediss"} else MQTT
        return transport(config.uri, config.namespace, types)

    def __enter__(self):
        self._start()
        return self

    def _start(self, *, emit=None):
        if self._started:
            raise RuntimeError("A service instance can only be started once")
        self._started = True
        try:
            if self.config.service_id == "":
                self.config.service_id = stable_identity(self.service_type, self.config.instance)
            self.config.validate()
            self.onInit()
            self.config.validate()
            self._registration = self.describe()
            emitter = self._resolve_emitter(emit) if emit is not None else None
            self._devices = {d["id"]: d for d in self._registration["devices"]}
            for device, tree in self._state.items():
                for attribute, record in tree.items():
                    self._check_attribute(device, attribute, record["value"])
            types = () if "*" in self._subscriptions else tuple(sorted(self._subscriptions | set(CONTROL_TYPES)))
            transport = self._injected_transport
            if transport is None:
                transport = self.transport_factory(self.config, types)
            self._client = Client(transport, self.config.service_id, self.service_type, self.config.encoding)
            self._register()
            self._registered = True
            if self._resync.is_set():
                self._resync.clear()
                self._queue_snapshot()
            self._flush_state()
            self._heartbeat()
            for target in (self._receive, self._publish, self._run_actions, self._run_hooks):
                self._start_worker(target)
            self.onStart()
            if (emit is None and not self.stopping and self.config.poll_interval
                    and type(self).onPoll is not Service.onPoll):
                self._start_worker(self._run_poll)
            return emitter
        except BaseException:
            try:
                self.close()
            except Exception:
                log.exception("Cleanup after service startup failed")
            raise

    def __exit__(self, *exc):
        self.close()

    def _resolve_emitter(self, name):
        matches = [(method, cap) for method, cap in self._events.items() if cap.name == name]
        if len(matches) != 1:
            raise ValueError(f"--emit needs a unique event name, got {name!r}")
        method, cap = matches[0]
        try:
            cap.signature.bind()
        except TypeError:
            raise ValueError(f"Event {name!r} requires arguments") from None
        return getattr(self, method)

    def _start_worker(self, target):
        thread = Thread(target=target, daemon=True, name=f"chains-{target.__name__}")
        thread.start()
        self._threads.append(thread)

    def _run_poll(self):
        interval = self.config.poll_interval
        while not self.stopping:
            try:
                self.onPoll()
            except Exception:
                log.exception("Service poll failed")
            if self._stop.wait(interval):
                return

    def run(self, *, emit=None):
        """Run until stopped, or invoke one declared event and shut down."""
        main = current_thread() is main_thread()
        previous = signal.getsignal(signal.SIGTERM) if main else None
        if main:
            signal.signal(signal.SIGTERM, lambda *_: self.stop())
        try:
            emitter = self._start(emit=emit)
            try:
                if emitter is None:
                    self._stop.wait()
                elif not self.stopping:
                    emitter()
            finally:
                self.close()
        except KeyboardInterrupt:
            pass
        finally:
            if main:
                signal.signal(signal.SIGTERM, previous)

    def stop(self):
        self._stop.set()
        self._wake.set()

    def close(self):
        if self._closed:
            return
        self._closed = True
        self.stop()
        with self._query_lock:
            for response in self._queries.values():
                if response.empty():
                    response.put_nowait(ConnectionError("Service is shutting down"))
        for thread in self._threads:
            if thread.ident != get_ident():
                thread.join()
        try:
            self.onShutdown()
        finally:
            if self._client is not None:
                try:
                    if self._registered:
                        self._flush_state()
                except Exception:
                    log.warning("Pending state could not be synchronized at shutdown")
                finally:
                    self._client.transport.close()

    @property
    def stopping(self):
        return self._stop.is_set()

    def _register(self):
        with self._send_lock:
            self._client.send(message("service.registered", self.config.service_id, self._registration))

    def register_device(self, device):
        """Publish an updated description before using a newly discovered device."""
        device = deepcopy(device)
        device_id = device["id"]
        if not IDENTIFIER.fullmatch(device_id):
            raise ValueError("Invalid device description")
        self._normalize_placement(device)
        device.setdefault("classes", [])
        device["attributes"] = self._attribute_descriptions(device.get("attributes", {}))
        device.setdefault("actions", ["get_state"])
        with self._send_lock:
            if device_id in self._devices:
                return
            registration = {**self._registration, "devices": [*self._registration["devices"], device]}
            self._client.send(message("service.registered", self.config.service_id, registration))
            self._registration = registration
            self._devices[device_id] = device

    def _send(self, value):
        if self._client is None:
            raise RuntimeError("Service has not connected")
        with self._send_lock:
            self._client.send(value)

    def _event_attribute(self, method):
        return self.event_attributes.get(method, "last_" + method.removeprefix("event_"))

    def _send_event(self, *, _device=None, _values=None, **fields):
        context = _event.get()
        if context is None or context[0] is not self:
            raise RuntimeError("_send_event() must be called in an event_ method")
        _, method, arguments = context
        self._publish_observation(self._prepare_event(method, {**arguments, **fields},
                                                      device=_device, values=_values))

    @staticmethod
    def _action_context():
        """Return a serializable snapshot of the active action, if any."""
        return deepcopy(_action.get())

    def _prepare_event(self, method, data, *, device=None, values=None, cause=None, event_id=None):
        """Build a validated observation envelope for deferred publication."""
        cap = self._events[method]
        device = device or cap.device or self._default_device
        if values is None:
            values = {self._event_attribute(method): data}
        return self._prepare_observations(cap.name,
            [{"device_id": device, "values": values, "data": data}],
            cap.version, cause=cause, event_id=event_id)

    def observe_batch(self, kind, observations, *, schema_version="1"):
        """Publish device patches under one event type and one central revision."""
        self._publish_observation(self._prepare_observations(kind, observations, schema_version))

    def _prepare_observations(self, kind, observations, version, *, cause=None, event_id=None):
        cause = cause or _action.get()
        metadata = caused_by(cause) if cause else {}
        if cause and cause.get("action_id"):
            metadata["action_id"] = cause["action_id"]
        cap = next((cap for cap in self._events.values()
                    if cap.name == kind and cap.version == version), None)
        if cap is None and kind != "device.observed":
            raise ValueError("Unknown event schema")
        seen = set()
        if not observations:
            raise ValueError("Empty observation batch")
        for patch in observations:
            device, values = patch["device_id"], patch["values"]
            if device not in self._devices or device in seen or not isinstance(values, dict) or not values:
                raise ValueError("Invalid observation device or values")
            seen.add(device)
            for name, value in values.items():
                self._check_attribute(device, name, value)
            if cap is not None:
                cap.validator.validate(patch.get("data", values))
        result = observation_message(self.config.service_id, kind, observations,
                                     schema_version=version, **metadata)
        if event_id is not None:
            if not IDENTIFIER.fullmatch(event_id):
                raise ValueError("Invalid event ID")
            result["id"] = event_id
        return result

    def _publish_observation(self, envelope):
        self._send(envelope)
        stamp = envelope["observed_at"]
        at = datetime.fromisoformat(stamp)
        observations = envelope["data"]["observations"]
        with self._state_lock:
            for patch in observations:
                current = self._state.get(patch["device_id"], {})
                if any(name in current and at < datetime.fromisoformat(current[name]["observed_at"])
                       for name in patch["values"]):
                    return
            for patch in observations:
                target = self._state.setdefault(patch["device_id"], {})
                for name, value in patch["values"].items():
                    old = target.get(name)
                    same = old and json.dumps(old["value"], sort_keys=True) == json.dumps(value, sort_keys=True)
                    changed = old["changed_at"] if same else stamp
                    target[name] = {"value": deepcopy(value), "observed_at": stamp, "changed_at": changed}

    def _heartbeat(self):
        interval = self.config.heartbeat_interval
        self._send(message("service.heartbeat", self.config.service_id,
                           {"instance_id": self._instance_id, "interval_seconds": interval},
                           expires_at=(utcnow() + timedelta(seconds=3 * interval)).isoformat()))

    def _check_attribute(self, device, attribute, value):
        descriptor = self._devices.get(device, {}).get("attributes", {}).get(attribute)
        if descriptor is None:
            raise ValueError(f"Undeclared state attribute {device}/{attribute}")
        Draft202012Validator(descriptor["schema"]).validate(value)

    def set_state(self, device=None, /, **attributes):
        self._update_state(device or getattr(self, "_default_device", DEFAULT_DEVICE), False, utcnow(), attributes)

    def seed_state(self, device=None, /, *, observed_at=None, **attributes):
        self._update_state(device or getattr(self, "_default_device", DEFAULT_DEVICE), True, utcnow() if observed_at is None else observed_at, attributes)

    def _update_state(self, device, silent, observed_at, attributes, *, restored_change=None):
        if not isinstance(observed_at, datetime) or observed_at.utcoffset() is None:
            raise ValueError("State observation time must be a timezone-aware datetime")
        if not IDENTIFIER.fullmatch(device) or any(not IDENTIFIER.fullmatch(k) or k == "_attributes" for k in attributes):
            raise ValueError("Invalid device or attribute name")
        # Validate and copy the wire value before changing the local state.
        wire = json.dumps(attributes, allow_nan=False)
        if len(wire) > MAX_PAYLOAD - 2048:
            raise ValueError("State update exceeds the message limit")
        values = json.loads(wire)
        if hasattr(self, "_devices"):
            for name, value in values.items():
                self._check_attribute(device, name, value)
        stamp = observed_at.astimezone(timezone.utc)
        cause = _action.get() if not silent else None
        with self._state_lock:
            target = self._state.setdefault(device, {})
            updates = {}
            for name, value in values.items():
                old = target.get(name)
                if old and stamp < datetime.fromisoformat(old["observed_at"]):
                    continue
                changed_at = stamp.isoformat()
                if restored_change is not None and restored_change <= stamp:
                    changed_at = restored_change.isoformat()
                if old and json.dumps(old["value"], sort_keys=True) == json.dumps(value, sort_keys=True):
                    changed_at = old["changed_at"]
                record = {"value": value, "changed_at": changed_at, "observed_at": stamp.isoformat()}
                target[name] = record
                updates[name] = value
            if updates:
                if len(self._pending) >= 4096:
                    self._resync.set()
                elif not self._resync.is_set():
                    self._pending.append((device, stamp.isoformat(), silent, cause, updates))
        self._wake.set()

    def get_state(self, device_id=None, attribute=None):
        if attribute is not None and device_id is None:
            raise ValueError("An attribute lookup requires a device")
        with self._state_lock:
            result = self._state if device_id is None else self._state.get(device_id, {})
            if attribute is not None:
                result = result.get(attribute)
            return deepcopy(result)

    def load_state(self, path):
        """Opt in to restoring saved state as silent observations."""
        try:
            snapshot = json.loads(Path(path).read_text(encoding="utf-8"))
        except FileNotFoundError:
            return
        for device, tree in snapshot.items():
            for attribute, record in tree.items():
                self._update_state(device, True, datetime.fromisoformat(record["observed_at"]),
                                   {attribute: record["value"]},
                                   restored_change=datetime.fromisoformat(record["changed_at"]))

    def save_state(self, path):
        """Opt in to storing a local state snapshot for a later startup."""
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with NamedTemporaryFile(dir=path.parent, delete=False, mode="w", encoding="utf-8") as tmp:
            temp = Path(tmp.name)
            try:
                json.dump(self.get_state(), tmp)
                tmp.flush()
                os.replace(temp, path)
            finally:
                temp.unlink(missing_ok=True)

    def _queue_snapshot(self):
        with self._state_lock:
            groups = {}
            for device, tree in self._state.items():
                for name, record in tree.items():
                    groups.setdefault((device, record["observed_at"]), {})[name] = record["value"]
            self._pending = deque((device, stamp, True, None, updates)
                                  for (device, stamp), updates in groups.items())

    def _flush_state(self, limit=None):
        sent = 0
        while limit is None or sent < limit:
            with self._state_lock:
                if not self._pending or self._resync.is_set():
                    return
                report = self._pending[0]
            device, stamp, silent, cause, updates = report
            meta = caused_by(cause) if cause else {}
            if cause and cause.get("action_id"):
                meta["action_id"] = cause["action_id"]
            if silent:
                self._send(message("state.reported", self.config.service_id, updates,
                                   device_id=device, observed_at=stamp, silent=True))
            else:
                self._send(observation_message(self.config.service_id, "device.observed",
                           [{"device_id": device, "values": updates}], observed_at=stamp, **meta))
            with self._state_lock:
                if self._pending and self._pending[0] is report:
                    self._pending.popleft()
            sent += 1
        self._wake.set()

    def query_state(self, service_id=None, device_id=None, attribute=None, *, timeout=5):
        """Return the authority reply, including found, state, and service availability."""
        if self._client is None or self.stopping:
            raise RuntimeError("Service must be connected to query central state")
        if timeout <= 0:
            raise ValueError("Query timeout must be positive")
        if attribute is not None and not device_id:
            raise ValueError("An attribute lookup requires a device")
        request = message("state.query.requested", self.config.service_id,
                          {"service_id": service_id or self.config.service_id,
                           "device_id": device_id or "", "attribute": attribute or ""},
                          target=self.config.authority,
                          expires_at=(utcnow() + timedelta(seconds=timeout)).isoformat())
        response = Queue(1)
        with self._query_lock:
            self._queries[request["id"]] = response
        end = monotonic() + timeout
        try:
            try:
                self._outgoing_queries.put_nowait((request, end))
                self._wake.set()
            except Full:
                raise RuntimeError("Too many pending state queries") from None
            try:
                data = response.get(timeout=max(0, end - monotonic()))
            except Empty:
                raise TimeoutError("Chains state authority did not respond") from None
            if isinstance(data, Exception):
                raise data
            if data.get("error"):
                raise ValueError(data["error"])
            return data
        finally:
            with self._query_lock:
                del self._queries[request["id"]]

    def _publish_queries(self):
        for _ in range(8):
            try:
                request, deadline = self._outgoing_queries.get_nowait()
            except Empty:
                return
            if monotonic() >= deadline:
                continue
            try:
                self._send(request)
            except Exception:
                with self._query_lock:
                    query = self._queries.get(request["id"])
                    if query is not None and query.empty():
                        query.put_nowait(ConnectionError("Could not publish Chains state query"))
                raise
        if not self._outgoing_queries.empty():
            self._wake.set()

    def _receive(self):
        try:
            while not self._stop.is_set():
                delivery = self._client.transport.receive(0.2)
                if delivery is None:
                    continue
                if delivery.reset:
                    self._resync.set()
                    self._wake.set()
                    continue
                if delivery.retained:
                    continue
                try:
                    packet = decode(delivery.data)
                    if route(packet) != delivery.topic:
                        continue
                    expires = packet.get("expires_at")
                    if expires and utcnow() >= datetime.fromisoformat(expires):
                        continue
                    kind = packet["type"]
                    if kind == "state.query.responded" and packet.get("target") == self.config.service_id and packet["source"] == self.config.authority:
                        with self._query_lock:
                            query = self._queries.get(packet.get("causation_id"))
                            if query is not None and query.empty() and isinstance(packet["data"], dict):
                                query.put_nowait(packet["data"])
                    elif kind == "action.requested" and packet.get("target") == self.config.service_id:
                        self._actions_queue.put_nowait(packet)
                    elif kind in self._subscriptions or ("*" in self._subscriptions and kind not in INTERNAL_TYPES):
                        self._hooks_queue.put_nowait((self.onEvent, (packet,)))
                except Full:
                    log.warning("Service receive queue is full")
                except (KeyError, ValueError, TypeError, OverflowError):
                    log.debug("Ignoring malformed Chains delivery")
        except Exception:
            if not self._stop.is_set():
                log.exception("Service receiver stopped")
                self.stop()

    def _publish(self):
        next_heartbeat = monotonic() + self.config.heartbeat_interval
        reconnect_hook = False
        while not self._stop.is_set():
            self._wake.wait(0.2)
            self._wake.clear()
            try:
                if self._resync.is_set():
                    self._resync.clear()
                    self._register()
                    self._queue_snapshot()
                    reconnect_hook = True
                    next_heartbeat = 0
                if monotonic() >= next_heartbeat:
                    self._heartbeat()
                    next_heartbeat = monotonic() + self.config.heartbeat_interval
                self._flush_state(limit=32)
                self._publish_queries()
                with self._state_lock:
                    empty = not self._pending
                if reconnect_hook and empty:
                    self._hooks_queue.put_nowait((self.onReconnect, ()))
                    reconnect_hook = False
            except Full:
                log.warning("Service lifecycle hook queue is full")
                reconnect_hook = False
            except Exception:
                if not self._stop.is_set():
                    log.warning("Service bus publication failed; retrying silently")
                    self._resync.set()
                    self._stop.wait(1)

    def _outcome(self, request, status, error="", **data):
        with self._send_lock:
            self._client.outcome(request, status, error, **data)

    def _run_actions(self):
        recent = OrderedDict()
        while not self._stop.is_set():
            try:
                request = self._actions_queue.get(timeout=0.2)
            except Empty:
                continue
            try:
                expires = request.get("expires_at")
                if expires and utcnow() >= datetime.fromisoformat(expires):
                    continue
                action = request["data"]
                key = (request["source"], request["action_id"])
                if key not in recent:
                    recent[key] = self._invoke(request, action)
                    if len(recent) > 4096:
                        recent.popitem(last=False)
                status, error, data = recent[key]
                self._outcome(request, status, error, **data)
            except Exception:
                if not self._stop.is_set():
                    log.warning("Could not send action outcome")

    def _invoke(self, request, action):
        token = _action.set(request)
        try:
            name, args = action["name"], action["args"]
            version, device = request["schema_version"], request["device_id"]
            if name not in self._devices.get(device, {}).get("actions", ()):
                raise ValueError("Action is not available on this device")
            if name == "get_state" and version == "1":
                Draft202012Validator(STATE_SCHEMA).validate(args)
                method = self.get_state
            else:
                cap, method = self._handlers[name, version]
                if (cap.device or self._default_device) != device:
                    raise ValueError("Action version is not available on this device")
                cap.validator.validate(args)
            self._outcome(request, "accepted")
            value = method(**args)
            # Fail cleanly when the result cannot be sent.
            encode_result = message(
                "action.outcome", self.config.service_id,
                {"status": "completed", "error": "", "value": value},
                target=request["source"], device_id=device, action_id=request["action_id"],
                **caused_by(request),
            )
            encode(encode_result, self.config.encoding)
            return "completed", "", {"value": value}
        except Exception as exc:
            log.debug("Action %s failed", request.get("action_id"), exc_info=True)
            return "failed", str(exc)[:500], {}
        finally:
            _action.reset(token)

    def _run_hooks(self):
        while not self._stop.is_set():
            try:
                hook, args = self._hooks_queue.get(timeout=0.2)
            except Empty:
                continue
            try:
                hook(*args)
            except Exception:
                log.exception("Service hook failed")


