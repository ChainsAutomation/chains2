"""Publish notifications to a configured ntfy server."""

from copy import deepcopy
from dataclasses import dataclass, field
from http.client import HTTPException
import json
from math import isfinite
import re
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from ..description import capability
from ..service import Service


_TOPIC = re.compile(r"[A-Za-z0-9_-]{1,64}")
_RESPONSE_LIMIT = 16_384
_NOTIFY_SCHEMA = {
    "title": "Send notification",
    "description": "Deliver a message to a configured ntfy topic.",
    "type": "object",
    "properties": {
        "message": {"type": "string", "minLength": 1, "maxLength": 4096},
        "topic": {"type": "string", "title": "Topic"},
        "title": {"type": "string", "maxLength": 1024},
        "priority": {"type": "integer", "minimum": 1, "maximum": 5},
        "tags": {"type": "array", "items": {"type": "string", "minLength": 1}},
        "click": {"type": "string", "title": "Open URL"},
    },
    "required": ["message"],
    "additionalProperties": False,
}


@dataclass
class Settings:
    server_url: str = ""
    token: str = field(default="", repr=False)
    topics: dict[str, str] = field(default_factory=lambda: {"alerts": "chains-alerts"})
    default_topic: str = "alerts"
    request_timeout: float = 5.0


def _http_url(value, *, origin_only=False):
    try:
        url = urlsplit(value)
        valid = (url.scheme in {"http", "https"} and url.hostname and url.port != 0
                 and not url.username and not url.password and "@" not in url.netloc
                 and not any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value))
        if valid and (not origin_only or (url.path in {"", "/"} and "?" not in value and "#" not in value)):
            return url
    except ValueError:
        pass
    return None


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Ntfy(Service):
    """Publish notifications to explicitly configured ntfy topics."""

    service_type = "ntfy"
    settings_type = Settings

    def describe_devices(self):
        return [{"id": "notifications", "name": "Notifications",
                 "description": "Send messages to configured ntfy topics.", "attributes": {}}]

    def describe_action_schema(self, name, version, schema):
        if name != "notify":
            return super().describe_action_schema(name, version, schema)
        configured = deepcopy(schema)
        configured["properties"]["topic"].update(enum=sorted(self.settings.topics),
                                                 default=self.settings.default_topic)
        return configured

    def onInit(self):
        settings = self.settings
        server = _http_url(settings.server_url, origin_only=True)
        if server is None:
            raise ValueError("Set ntfy server_url to an HTTP(S) origin without credentials")
        if not settings.topics or settings.default_topic not in settings.topics:
            raise ValueError("Set ntfy default_topic to one of the configured topic aliases")
        if any(not _TOPIC.fullmatch(alias) or not _TOPIC.fullmatch(topic)
               for alias, topic in settings.topics.items()):
            raise ValueError("Invalid ntfy topic name or alias")
        if not isfinite(settings.request_timeout) or not 0 < settings.request_timeout <= 60:
            raise ValueError("ntfy request_timeout must be positive and at most 60 seconds")
        if settings.token and any(not 33 <= ord(char) <= 126 for char in settings.token):
            raise ValueError("Invalid ntfy token")
        self._endpoint = f"{server.scheme}://{server.netloc}/"
        self._opener = build_opener(ProxyHandler({}), _NoRedirect())

    @capability(schema=_NOTIFY_SCHEMA)
    def action_notify(self, message: str, topic: str | None = None, title: str | None = None,
                      priority: int | None = None, tags: list[str] | None = None, click: str | None = None):
        alias = self.settings.default_topic if topic is None else topic
        if alias not in self.settings.topics:
            raise ValueError("Select a configured ntfy topic alias")
        if not message.strip() or len(message.encode("utf-8")) > 4096:
            raise ValueError("ntfy message must have text and fit in 4096 UTF-8 bytes")
        if click is not None and _http_url(click) is None:
            raise ValueError("ntfy click must be an HTTP(S) URL without credentials")
        target = self.settings.topics[alias]
        body = {"topic": target, "message": message}
        body.update({key: value for key, value in {"title": title, "priority": priority,
                                                   "tags": tags, "click": click}.items() if value is not None})
        headers = {"Content-Type": "application/json", "Accept": "application/json"}
        if self.settings.token:
            headers["Authorization"] = f"Bearer {self.settings.token}"
        request = Request(self._endpoint, data=json.dumps(body).encode(), headers=headers, method="POST")
        try:
            with self._opener.open(request, timeout=self.settings.request_timeout) as response:
                raw = response.read(_RESPONSE_LIMIT + 1)
        except HTTPError as error:
            error.close()
            raise ValueError(f"ntfy rejected the publish (HTTP {error.code})") from None
        except (OSError, HTTPException, ValueError):
            raise ValueError("ntfy publish request failed or timed out") from None
        try:
            receipt = json.loads(raw) if len(raw) <= _RESPONSE_LIMIT else None
        except (ValueError, RecursionError):
            receipt = None
        if (not isinstance(receipt, dict) or not isinstance(receipt.get("id"), str) or not receipt["id"]
                or len(receipt["id"]) > 128 or receipt.get("topic") != target
                or type(receipt.get("time")) is not int):
            raise ValueError("ntfy returned an invalid publish receipt")
        return {"id": receipt["id"], "topic": alias, "time": receipt["time"]}
