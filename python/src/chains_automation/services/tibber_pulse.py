"""Read meter data from an already configured Tibber Pulse IR Bridge."""

from base64 import b64encode
from collections import OrderedDict
from dataclasses import dataclass, field
from http.client import HTTPException
import logging
from math import isfinite
from urllib.error import HTTPError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from ..description import capability
from ..service import Service
from ._sml import ATTRIBUTES, MAX_TELEGRAM_RESPONSE, decode_readings


log = logging.getLogger(__name__)
_ENDPOINTS = ("/node_data.json", "/data.json")
_READING_SCHEMA = {
    "type": "object",
    "properties": {name: {"type": "number"} for name in ATTRIBUTES},
    "additionalProperties": False,
    "minProperties": 1,
}


@dataclass
class Settings:
    bridge_url: str = ""
    username: str = "admin"
    password: str = field(default="", repr=False)
    node_id: int = 1
    request_timeout: float = 5.0


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class PulseBridge:
    def __init__(self, settings):
        try:
            url = urlsplit(settings.bridge_url)
            port = url.port
        except ValueError:
            raise ValueError("Invalid Tibber Bridge URL") from None
        if (url.scheme not in {"http", "https"} or not url.hostname or not url.netloc
                or "@" in url.netloc or url.path not in {"", "/"} or url.query or url.fragment
                or port == 0 or any(char.isspace() for char in settings.bridge_url)):
            raise ValueError("Set bridge_url to the local HTTP(S) origin without credentials")
        if not settings.username or ":" in settings.username or not settings.password:
            raise ValueError("Set the Tibber Bridge username and password")
        if settings.node_id < 1:
            raise ValueError("Tibber node_id must be positive")
        if not isfinite(settings.request_timeout) or not 0 < settings.request_timeout <= 60:
            raise ValueError("Tibber request_timeout must be between 0 and 60 seconds")
        self._origin = f"{url.scheme}://{url.netloc}"
        basic = b64encode(f"{settings.username}:{settings.password}".encode()).decode("ascii")
        self._authorization = f"Basic {basic}"
        self._timeout = settings.request_timeout
        self._query = urlencode({"node_id": settings.node_id})
        self._opener = build_opener(ProxyHandler({}), _NoRedirect())
        self._preferred = _ENDPOINTS[0]

    def read(self):
        fallback = next(path for path in _ENDPOINTS if path != self._preferred)
        for path in (self._preferred, fallback):
            request = Request(f"{self._origin}{path}?{self._query}",
                              headers={"Authorization": self._authorization, "Cache-Control": "no-cache"})
            try:
                with self._opener.open(request, timeout=self._timeout) as response:
                    content_type = response.headers.get_content_type()
                    data = response.read(MAX_TELEGRAM_RESPONSE + 1)
            except HTTPError as error:
                with error:
                    status = error.code
                if status in {404, 405, 410}:
                    continue
                raise ValueError(f"Tibber Bridge returned HTTP {status}") from None
            except (OSError, HTTPException):
                raise ValueError("Tibber Bridge request failed or timed out") from None
            if len(data) > MAX_TELEGRAM_RESPONSE:
                raise ValueError("Tibber Bridge response exceeds size limit")
            prefix = data[:64].lstrip().lower()
            if (prefix.startswith((b"<!doctype html", b"<html", b"<head"))
                    or (content_type == "text/html" and prefix.startswith(b"<"))):
                continue
            telegrams = decode_readings(data)
            self._preferred = path
            return telegrams
        raise ValueError("No supported local meter endpoint on the Tibber Bridge")


class TibberPulse(Service):
    service_type = "tibber_pulse"
    settings_type = Settings
    event_attributes = {"event_reading": None}

    def describe_devices(self):
        return [{"id": "default", "name": "Electricity meter", "class": "power_meter",
                 "attributes": list(ATTRIBUTES)}]

    def onInit(self):
        self._bridge = PulseBridge(self.settings)
        if self.config.poll_interval:
            self.config.poll_interval = max(3.0, self.config.poll_interval)
        self._published = OrderedDict()
        self._last_batch = ()
        self._last_error = ""

    def onPoll(self):
        try:
            telegrams = self._bridge.read()
        except ValueError as error:
            reason = str(error)
            if reason != self._last_error:
                log.warning("Tibber Pulse poll failed: %s", reason)
            self._last_error = reason
            return
        if self._last_error:
            log.info("Tibber Pulse readings resumed")
            self._last_error = ""

        batch = tuple(fingerprint for fingerprint, _ in telegrams)
        if batch == self._last_batch:
            return
        for fingerprint, measurements in telegrams:
            if fingerprint in self._published:
                continue
            self.event_reading(measurements)
            self._published[fingerprint] = None
            if len(self._published) > 64:
                self._published.popitem(last=False)
        self._last_batch = batch

    @capability(name="tibber_pulse.reading", schema=_READING_SCHEMA)
    def event_reading(self, measurements: dict[str, int | float]):
        self._publish_observation(self._prepare_event("event_reading", measurements, values=measurements))
