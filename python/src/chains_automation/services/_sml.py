"""Decode the SML transport v1 meter readings returned by a Pulse IR Bridge."""

from hashlib import sha256


_ESCAPE = b"\x1b" * 4
_START = _ESCAPE + b"\x01" * 4
_GET_LIST_RESPONSE = 0x0701
MAX_TELEGRAM_RESPONSE = 128 * 1024


def _metrics():
    # OBIS groups C, D and E for electricity on channel zero.
    readings = {
        (16, 7, 0): ("power_w", 27),
        (1, 7, 0): ("power_import_w", 27),
        (2, 7, 0): ("power_export_w", 27),
        (1, 8, 0): ("energy_import_wh", 30),
        (2, 8, 0): ("energy_export_wh", 30),
    }
    for phase in range(1, 4):
        offset = (phase - 1) * 20
        readings[(36 + offset, 7, 0)] = (f"l{phase}_power_w", 27)
        readings[(32 + offset, 7, 0)] = (f"l{phase}_voltage_v", 35)
        readings[(31 + offset, 7, 0)] = (f"l{phase}_current_a", 33)
    return {bytes((1, 0, *code, 255)): metric for code, metric in readings.items()}


_METRICS = _metrics()
ATTRIBUTES = tuple(field for field, _ in _METRICS.values())


def _crc16(data):
    """CRC-16/X-25 as used by SML transport version 1."""
    crc = 0xffff
    for byte in data:
        crc ^= byte
        for _ in range(8):
            crc = (crc >> 1) ^ (0x8408 if crc & 1 else 0)
    return crc ^ 0xffff


def _payloads(data):
    """Yield checksum-verified payloads from a finite HTTP response."""
    cursor = 0
    while (start := data.find(_START, cursor)) >= 0:
        body = bytearray()
        pos = start + len(_START)
        cursor = pos
        while (escape := data.find(_ESCAPE, pos)) >= 0:
            body.extend(data[pos:escape])
            tail = escape + len(_ESCAPE)
            if data.startswith(_ESCAPE, tail):
                body.extend(_ESCAPE)
                pos = tail + len(_ESCAPE)
                cursor = pos
                continue
            if data[tail:tail + 1] == b"\x1a" and tail + 4 <= len(data):
                pending = data[tail + 1]
                crc_at = tail + 2
                cursor = crc_at + 2
                if (pending <= 3 and pending <= len(body)
                        and (not pending or body[-pending:] == b"\x00" * pending)
                        and _crc16(memoryview(data)[start:crc_at]) == int.from_bytes(data[crc_at:cursor], "little")):
                    if pending:
                        del body[-pending:]
                    yield bytes(body)
            else:
                # An incomplete frame may be followed by a new transport header.
                cursor = escape if data.startswith(_START, escape) else tail
            break
        else:
            return


class _TLReader:
    """Read SML's type-length encoding, including lists and optional values."""

    def __init__(self, payload):
        self.payload = payload
        self.pos = 0

    def take(self, count):
        end = self.pos + count
        if end > len(self.payload):
            raise ValueError("Truncated SML item")
        data = self.payload[self.pos:end]
        self.pos = end
        return data

    def value(self, depth=0):
        if depth >= 16:
            raise ValueError("SML list nesting exceeds limit")
        first = self.take(1)[0]
        kind = first & 0x70
        count = first & 0x0f
        size = 1
        header = first
        while header & 0x80:
            header = self.take(1)[0]
            if header & 0x70 or size == 4:
                raise ValueError("Invalid SML type-length encoding")
            count = (count << 4) | (header & 0x0f)
            size += 1

        if kind == 0x70:
            if count > len(self.payload) - self.pos:
                raise ValueError("Truncated SML list")
            return [self.value(depth + 1) for _ in range(count)]
        if count < size:
            raise ValueError("Invalid SML scalar length")
        content = self.take(count - size)
        if kind == 0:
            return content or None
        if kind in (0x50, 0x60) and 1 <= len(content) <= 8:
            return int.from_bytes(content, "big", signed=kind == 0x50)
        if kind == 0x40 and len(content) == 1:
            return bool(content[0])
        raise ValueError("Unsupported SML data type")


def _readings(payload):
    parser = _TLReader(payload)
    values = {}
    while parser.pos < len(payload):
        # The sixth SML message field is a literal EndOfSmlMsg byte, not a TL scalar.
        if parser.take(1) != b"\x76":
            raise ValueError("Invalid SML message header")
        _, _, _, body, checksum = (parser.value() for _ in range(5))
        if type(checksum) is not int or not 0 <= checksum <= 0xffff or parser.take(1) != b"\x00":
            raise ValueError("Invalid SML message footer")
        if not isinstance(body, list) or len(body) != 2:
            raise ValueError("Invalid SML message body")
        if body[0] != _GET_LIST_RESPONSE:
            continue
        response = body[1]
        if not isinstance(response, list) or len(response) != 7 or not isinstance(response[4], list):
            raise ValueError("Invalid SML GetList response")
        for entry in response[4]:
            if not isinstance(entry, list) or len(entry) != 7 or not isinstance(entry[0], bytes):
                raise ValueError("Invalid SML list entry")
            metric = _METRICS.get(entry[0])
            if metric is None:
                continue
            name, expected_unit = metric
            unit, scaler, number = entry[3:6]
            if scaler is None:
                scaler = 0
            if unit == expected_unit and type(number) is int and type(scaler) is int and -12 <= scaler <= 12:
                values[name] = number * 10 ** scaler if scaler >= 0 else number / 10 ** -scaler

    if "power_w" not in values and {"power_import_w", "power_export_w"} <= values.keys():
        values["power_w"] = values["power_import_w"] - values["power_export_w"]
    return values


def decode_readings(data):
    """Return (fingerprint, measurements) pairs in wire order."""
    if len(data) > MAX_TELEGRAM_RESPONSE:
        raise ValueError("SML response exceeds size limit")
    readings = []
    for payload in _payloads(data):
        try:
            values = _readings(payload)
        except ValueError:
            continue
        if values:
            readings.append((sha256(payload).digest(), values))
    if not readings:
        raise ValueError("No valid SML telegram with supported readings")
    return readings
