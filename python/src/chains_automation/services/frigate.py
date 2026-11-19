"""Bridge Frigate tracked-object and review updates into Chains events."""

from dataclasses import dataclass, field
import json
import logging
from typing import Any
from urllib.parse import urlsplit

from ._reader import ReaderService
from ..description import capability
from ..transports import MQTT


log = logging.getLogger(__name__)
TOPICS = frozenset({"events", "reviews"})


@dataclass
class FrigateSettings:
    mqtt_uri: str | None = field(default=None, metadata={"help": "Frigate's MQTT broker URI"})
    topic_prefix: str = "frigate"
    topics: list[str] = field(default_factory=lambda: ["events", "reviews"])
    reconnect_interval: float = 2.0


def parse_update(raw):
    """Extract matchable fields while preserving the complete native update."""
    update = json.loads(raw)
    if not isinstance(update, dict) or update.get("type") not in ("new", "update", "end"):
        return None
    after = update.get("after")
    if not isinstance(after, dict):
        return None
    camera, native_id = after.get("camera"), after.get("id")
    if not isinstance(camera, str) or not camera or not isinstance(native_id, str) or not native_id:
        return None
    label, severity = after.get("label"), after.get("severity")
    data = after.get("data")
    zones = after.get("current_zones", data.get("zones", []) if isinstance(data, dict) else [])
    return {
        "phase": update["type"],
        "camera": camera,
        "native_id": native_id,
        "label": label if isinstance(label, str) else None,
        "severity": severity if isinstance(severity, str) else None,
        "zones": [zone for zone in zones if isinstance(zone, str)] if isinstance(zones, list) else [],
        "payload": update,
    }


class Frigate(ReaderService):
    settings_type = FrigateSettings

    def onInit(self):
        self._source_uri = self.settings.mqtt_uri if self.settings.mqtt_uri is not None else self.config.uri
        try:
            uri = urlsplit(self._source_uri)
            port = uri.port
        except ValueError:
            raise ValueError("Invalid Frigate MQTT URI") from None
        unix = uri.scheme == "mqtt+unix" and not uri.netloc and uri.path.startswith("/")
        tcp = uri.scheme in {"mqtt", "mqtts"} and uri.hostname and port != 0
        if not (unix or tcp):
            raise ValueError("Frigate needs an MQTT source URI; set settings.mqtt_uri when using Redis")
        prefix = self.settings.topic_prefix
        if not prefix or prefix != prefix.strip("/") or any(c in prefix for c in "+#\x00"):
            raise ValueError("Frigate topic_prefix must be a concrete MQTT prefix")
        if not self.settings.topics or any(topic not in TOPICS for topic in self.settings.topics):
            raise ValueError("Frigate topics must contain events, reviews, or both")
        self.validate_reconnect_interval(self.settings.reconnect_interval)
        handlers = {"events": self.event_object, "reviews": self.event_review}
        self._handlers_by_topic = {f"{prefix}/{topic}": handlers[topic] for topic in self.settings.topics}

    def describe_devices(self):
        return [{"id": "default", "name": "Frigate", "class": "camera_bridge", "attributes": []}]

    def read_source(self):
        mqtt = MQTT(self._source_uri, raw_topics=tuple(self._handlers_by_topic))
        try:
            while not self.stopping:
                delivery = mqtt.receive(0.2)
                if delivery is None or delivery.retained:
                    continue
                if delivery.reset:
                    log.warning("Frigate MQTT stream continuity lost")
                    continue
                handler = self._handlers_by_topic.get(delivery.topic)
                if handler is None:
                    continue
                try:
                    update = parse_update(delivery.data)
                    if update is not None and not self.stopping:
                        self.publish_input(handler, update)
                except (ValueError, RecursionError):
                    log.debug("Ignoring invalid Frigate update")
        finally:
            mqtt.close()

    @capability(name="frigate.object")
    def event_object(self, phase: str, camera: str, native_id: str, label: str | None,
                     severity: str | None, zones: list[str], payload: dict[str, Any]):
        self._send_event()

    @capability(name="frigate.review")
    def event_review(self, phase: str, camera: str, native_id: str, label: str | None,
                     severity: str | None, zones: list[str], payload: dict[str, Any]):
        self._send_event()
