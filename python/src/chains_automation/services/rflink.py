"""Read RFLink USB receive frames and publish their decoded fields."""

from dataclasses import dataclass, field
import logging
import re

from ._reader import ReaderService
from ..description import capability


log = logging.getLogger(__name__)
MAX_LINE = 4096
_SEQUENCE = re.compile(r"[0-9a-fA-F]{2}")
_HEX = re.compile(r"[0-9a-fA-F]+")


@dataclass
class RFLinkSettings:
    port: str = field(metadata={"help": "RFLink serial device path"})
    baudrate: int = 57600
    reconnect_interval: float = 2.0


def _measurements(fields):
    values = {}
    for key, label in (("TEMP", "temperature_c"), ("WINTMP", "wind_temperature_c"),
                       ("WINCHL", "wind_chill_c")):
        raw = fields.get(key, "")
        if _HEX.fullmatch(raw):
            n = int(raw, 16)
            if n <= 0xffff:
                values[label] = (-1 if n & 0x8000 else 1) * (n & 0x7fff) / 10
    humidity = fields.get("HUM", "")
    if humidity.isascii() and humidity.isdecimal() and len(humidity) <= 3 and int(humidity) <= 100:
        values["humidity_percent"] = int(humidity)
    if fields.get("BAT") in {"OK", "LOW"}:
        values["battery_low"] = fields["BAT"] == "LOW"
    for key, label, scale in (
        ("BARO", "pressure", 1), ("LUX", "illuminance_lux", 1),
        ("RAIN", "rain_mm", 10), ("RAINRATE", "rain_rate", 10),
        ("WINSP", "wind_speed_kmh", 10), ("AWINSP", "average_wind_speed_kmh", 10),
    ):
        raw = fields.get(key, "")
        if len(raw) <= 8 and _HEX.fullmatch(raw):
            n = int(raw, 16)
            values[label] = n if scale == 1 else n / scale
    return values


def parse_frame(raw):
    """Return the event payload for a received RF frame, or None."""
    line = raw.decode("ascii").rstrip("\r\n")
    tokens = line.split(";")
    if len(tokens) < 4 or tokens[0] != "20" or not _SEQUENCE.fullmatch(tokens[1]) or not tokens[2]:
        return None
    fields = {}
    for token in tokens[3:]:
        name, sep, value = token.partition("=")
        if sep and name:
            fields[name.upper()] = value
    if not fields.get("ID"):
        return None
    return {
        "protocol": tokens[2],
        "sequence": int(tokens[1], 16),
        "rf_id": fields["ID"],
        "switch": fields.get("SWITCH"),
        "command": fields.get("CMD"),
        "fields": fields,
        "measurements": _measurements(fields),
        "raw": line,
    }


def _frames(port, stopping):
    line = bytearray()
    oversized = False
    while not stopping():
        for byte in port.read(min(max(port.in_waiting, 1), MAX_LINE)):
            if byte == 10:
                if not oversized:
                    yield bytes(line)
                line.clear()
                oversized = False
            elif not oversized:
                if len(line) == MAX_LINE:
                    oversized = True
                    log.warning("Discarding oversized RFLink line")
                else:
                    line.append(byte)


class RFLink(ReaderService):
    service_type = "rflink"
    settings_type = RFLinkSettings

    def onInit(self):
        if not self.settings.port.strip() or self.settings.baudrate <= 0:
            raise ValueError("RFLink requires a serial port and a positive baud rate")
        self.validate_reconnect_interval(self.settings.reconnect_interval)
        try:
            import serial
        except ImportError:
            raise ValueError("Install the rflink SDK extra to use the serial service") from None
        self._serial = serial

    def describe_devices(self):
        return [{"id": "system", "name": "RFLink gateway", "classes": [], "attributes": []}]

    def read_source(self):
        with self._serial.Serial(self.settings.port, self.settings.baudrate, timeout=0.2) as port:
            for line in _frames(port, lambda: self.stopping):
                try:
                    frame = parse_frame(line)
                    if frame is not None and not self.stopping:
                        self.publish_input(self.event_signal, frame)
                except UnicodeDecodeError:
                    log.debug("Ignoring non-ASCII RFLink input")

    @capability(name="rflink.signal")
    def event_signal(self, protocol: str, sequence: int, rf_id: str, switch: str | None,
                     command: str | None, fields: dict[str, str],
                     measurements: dict[str, float | bool], raw: str):
        self._send_event()
