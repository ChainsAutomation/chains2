"""Bridge Frigate tracked-object and review updates into Chains events."""

from dataclasses import dataclass, field
import json
from hashlib import sha256
import logging
from typing import Any
from urllib.parse import urlsplit

from ._reader import ReaderService
from ..description import capability
from ..client import IDENTIFIER
from ..transports import MQTT


log = logging.getLogger(__name__)
TOPICS = frozenset({"events", "reviews"})


@dataclass
class FrigateSettings:
    mqtt_uri: str | None = field(default=None, metadata={"help": "Frigate's MQTT broker URI"})
    topic_prefix: str = "frigate"
    topics: list[str] = field(default_factory=lambda: ["events", "reviews"])
    reconnect_interval: float = 2.0
    cameras: list[str] = field(default_factory=list)


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
    retain_devices = True

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
        feeds = {"events": self.event_object,
                 "reviews": self.event_review}
        self._feeds_by_topic = {f"{prefix}/{topic}": feeds[topic] for topic in self.settings.topics}

    event_attributes = {"event_object": None, "event_review": None}

    @staticmethod
    def camera_id(name):
        if name != "system" and not name.startswith("camera:") and IDENTIFIER.fullmatch(name):
            return name
        return "camera:" + sha256(name.encode()).hexdigest()

    @classmethod
    def _camera_description(cls, camera):
        return {"id": cls.camera_id(camera), "name": camera, "classes": [],
                "attributes": {name: {"schema": {"type": "object"}}
                               for name in ("last_object", "last_review")}}

    def describe_devices(self):
        if any(not camera.strip() for camera in self.settings.cameras):
            raise ValueError("Frigate camera names cannot be blank")
        return super().describe_devices() + [
            self._camera_description(camera) for camera in dict.fromkeys(self.settings.cameras)
        ]

    def _emit_camera(self, attribute, update, camera):
        description = self._camera_description(camera)
        self.register_device(description)
        self._send_event(_device=description["id"], _values={attribute: update})

    def read_source(self):
        mqtt = MQTT(self._source_uri, raw_topics=tuple(self._feeds_by_topic))
        try:
            while not self.stopping:
                delivery = mqtt.receive(0.2)
                if delivery is None or delivery.retained:
                    continue
                if delivery.reset:
                    log.warning("Frigate MQTT stream continuity lost")
                    continue
                feed = self._feeds_by_topic.get(delivery.topic)
                if feed is None:
                    continue
                try:
                    update = parse_update(delivery.data)
                    if update is not None and not self.stopping:
                        self.publish_input(feed, update)
                except (ValueError, RecursionError):
                    log.debug("Ignoring invalid Frigate update")
        finally:
            mqtt.close()

    @capability(name="frigate.object")
    def event_object(self, phase: str, camera: str, native_id: str, label: str | None,
                     severity: str | None, zones: list[str], payload: dict[str, Any]):
        summary = {"phase": phase, "native_id": native_id, "label": label,
                   "severity": severity, "zones": zones}
        self._emit_camera("last_object", summary, camera)

    @capability(name="frigate.review")
    def event_review(self, phase: str, camera: str, native_id: str, label: str | None,
                     severity: str | None, zones: list[str], payload: dict[str, Any]):
        summary = {"phase": phase, "native_id": native_id, "label": label,
                   "severity": severity, "zones": zones}
        self._emit_camera("last_review", summary, camera)
