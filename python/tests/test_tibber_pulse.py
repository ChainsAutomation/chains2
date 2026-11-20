from binascii import crc_hqx
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
from queue import Empty
from subprocess import DEVNULL, Popen
from tempfile import TemporaryDirectory
from threading import Thread
from time import monotonic, sleep
from unittest import TestCase, skipUnless
from uuid import uuid4

from chains_automation import decode
from chains_automation.service_config import ServiceConfig
from chains_automation.services._sml import MAX_TELEGRAM_RESPONSE, _crc16, decode_readings
from chains_automation.services.tibber_pulse import PulseBridge, Settings, TibberPulse
from test_service import Broker


CURRENT = "/node_data.json?node_id=2"
LEGACY = "/data.json?node_id=2"
OK = (200, "application/octet-stream")
START = b"\x1b" * 4 + b"\x01" * 4
ESC = b"\x1b" * 4


def wire_crc(raw):
    def reverse_byte(value):
        return int(f"{value:08b}"[::-1], 2)

    # Use the standard library's MSB-first CCITT routine with reflected bytes.
    crc = crc_hqx(bytes(map(reverse_byte, raw)), 0xffff)
    return (int(f"{crc:016b}"[::-1], 2) ^ 0xffff).to_bytes(2, "little")


def tl(kind, length):
    digits = [int(c, 16) for c in format(length, "x")]
    return bytes((kind if i == 0 else 0) | digit | (0x80 if i < len(digits) - 1 else 0)
                 for i, digit in enumerate(digits))


def item(*values):
    return tl(0x70, len(values)) + b"".join(values)


def octets(value):
    size = len(value) + 1
    while size - len(tl(0, size)) < len(value):
        size += 1
    return tl(0, size) + value


def number(value, signed=False, size=1):
    return tl(0x50 if signed else 0x60, size + 1) + value.to_bytes(size, "big", signed=signed)


def message(tag, body, sequence):
    prefix = b"\x76" + octets(bytes([sequence])) + number(0) + number(0) + item(number(tag, size=2), body)
    return prefix + b"\x63" + wire_crc(prefix) + b"\x00"


def frame(payload):
    body = payload.replace(ESC, ESC * 2)
    padding = (-len(body)) % 4
    raw = START + body + bytes(padding) + ESC + b"\x1a" + bytes([padding])
    return raw + wire_crc(raw)


def telegram(sequence=1, entries=()):
    encoded = []
    for c, d, unit, scaler, value in entries:
        code = octets(bytes((1, 0, c, d, 0, 255)))
        encoded.append(item(code, b"\x01", b"\x01", number(unit),
                            b"\x01" if scaler is None else number(scaler, True),
                            number(value, value < 0, 8), b"\x01"))
    get_list = item(b"\x01", octets(b"fixture" + ESC + b"x" * 16), b"\x01", b"\x01",
                    item(*encoded), b"\x01", b"\x01")
    return frame(message(0x0701, get_list, sequence) + message(0x0201, item(b"\x01"), sequence))


def power(seq, watts):
    return telegram(seq, [(16, 7, 27, 0, watts)])


@contextmanager
def bridge_server(callback):
    class BridgeHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            code, content_type, data = callback(self.path, self.headers.get("Authorization"))
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            if code == 302:
                self.send_header("Location", LEGACY)
            self.end_headers()
            try:
                self.wfile.write(data)
            except (BrokenPipeError, ConnectionResetError):
                pass

    with ThreadingHTTPServer(("127.0.0.1", 0), BridgeHandler) as server:
        thread = Thread(target=server.serve_forever, kwargs={"poll_interval": .02})
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}"
        finally:
            server.shutdown()
            thread.join()


class SMLTest(TestCase):
    def test_totals_and_phases(self):
        self.assertEqual(_crc16(b"123456789"), 0x906e)
        data = [
            (16, 7, 27, -1, -3501), (1, 7, 27, 0, 5), (2, 7, 27, 0, 355),
            (1, 8, 30, -1, 456789), (2, 8, 30, 0, 789),
            (36, 7, 27, 0, 50), (56, 7, 27, 0, -100), (76, 7, 27, 0, -300),
            (32, 7, 35, -1, 2307), (52, 7, 35, -1, 2315), (72, 7, 35, -1, 2280),
            (31, 7, 33, -2, 100), (51, 7, 33, -2, 155), (71, 7, 33, -2, 200),
            (99, 7, 27, 0, 999),
        ]
        _, values = decode_readings(telegram(entries=data))[0]
        self.assertEqual(values, {
            "power_w": -350.1, "power_import_w": 5, "power_export_w": 355,
            "energy_import_wh": 45678.9, "energy_export_wh": 789,
            "l1_power_w": 50, "l2_power_w": -100, "l3_power_w": -300,
            "l1_voltage_v": 230.7, "l2_voltage_v": 231.5, "l3_voltage_v": 228.0,
            "l1_current_a": 1.0, "l2_current_a": 1.55, "l3_current_a": 2.0,
        })

    def test_units_net_fallback_and_missing_phases(self):
        data = [(1, 7, 27, None, 10), (2, 7, 27, 0, 101), (52, 7, 33, 0, 234), (31, 7, 33, -30, 1)]
        self.assertEqual(decode_readings(telegram(entries=data))[0][1],
                         {"power_import_w": 10, "power_export_w": 101, "power_w": -91})
        self.assertEqual(decode_readings(telegram(entries=data[:1]))[0][1], {"power_import_w": 10})

    def test_malformed_and_multi_frame_recovery(self):
        valid = power(1, 42)
        corrupt = valid[:-1] + bytes([valid[-1] ^ 1])
        for data in (corrupt, valid[:-10], frame(b"\x76\x70"), frame(b"\x76\x80" * 20), b"x" * (MAX_TELEGRAM_RESPONSE + 1)):
            with self.subTest(size=len(data)), self.assertRaises(ValueError):
                decode_readings(data)
        data = b"junk" + corrupt + valid[:-10] + valid + power(2, 45)
        self.assertEqual([r["power_w"] for _, r in decode_readings(data)], [42, 45])


class PulseHTTPTest(TestCase):
    def test_authentication_and_endpoint_discovery(self):
        requests = []

        def handle(path, auth):
            requests.append((path, auth))
            if path == CURRENT:
                return 200, "text/plain", b"<!doctype html><title>Bridge</title>"
            return *OK, power(1, 42)

        with bridge_server(handle) as address:
            bridge = PulseBridge(Settings(address, password="fixture", node_id=2))
            self.assertEqual(bridge.read()[0][1], {"power_w": 42})
            self.assertEqual(bridge.read()[0][1], {"power_w": 42})
        self.assertEqual(requests, [(path, "Basic YWRtaW46Zml4dHVyZQ==")
                                    for path in (CURRENT, LEGACY, LEGACY)])

    def test_bad_credentials_redirect_and_invalid_telegram_do_not_fallback(self):
        requests = []
        responses = [(401, "text/plain", b"secret"), (302, "text/plain", b"secret"),
                     (*OK, b"malformed"), (*OK, b"x" * (MAX_TELEGRAM_RESPONSE + 1))]

        def handle(path, _auth):
            requests.append(path)
            return responses.pop(0)

        with bridge_server(handle) as address:
            client = PulseBridge(Settings(address, password="fixture", node_id=2))
            for reason in ("HTTP 401", "HTTP 302", "No valid SML", "size limit"):
                with self.assertRaisesRegex(ValueError, reason):
                    client.read()
        self.assertEqual(requests, [CURRENT] * 4)

    def test_timeout_recovery_and_legacy_upgrade(self):
        requests = []

        def handle(path, _auth):
            requests.append(path)
            if len(requests) == 1:
                sleep(.3)
            if ((path == CURRENT and len(requests) <= 2)
                    or (path == LEGACY and len(requests) == 4)):
                return 404, "text/plain", b""
            return *OK, power(1, 42)

        with bridge_server(handle) as address:
            client = PulseBridge(Settings(address, password="fixture", node_id=2, request_timeout=.12))
            with self.assertRaisesRegex(ValueError, "timed out"):
                client.read()
            self.assertEqual(client.read()[0][1], {"power_w": 42})
            self.assertEqual(client.read()[0][1], {"power_w": 42})
        self.assertEqual(requests, [CURRENT, CURRENT, LEGACY, LEGACY, CURRENT])

    def test_service_deduplicates_cached_telegrams_and_keeps_state(self):
        broker = Broker()
        data = telegram(entries=[(16, 7, 27, 0, 42), (32, 7, 35, 0, 230)])

        def handle(_path, _auth):
            return *OK, data

        with bridge_server(handle) as address, TibberPulse(
            transport=broker, service_id="meter", poll_interval=0,
            settings={"bridge_url": address, "password": "fixture"},
        ) as service:
            service.onPoll()
            first = broker.until_observation("tibber_pulse.reading")
            self.assertEqual(first["data"], {"power_w": 42, "l1_voltage_v": 230})
            voltage = service.get_state("default", "l1_voltage_v")
            sample = service.get_state("default", "power_w")
            service.onPoll()
            self.assertEqual(service.get_state("default", "power_w"), sample)
            data = b"bad response"
            with self.assertLogs(level="WARNING"):
                service.onPoll()
            self.assertEqual(service.get_state("default", "power_w"), sample)
            data = power(2, 42)
            service.onPoll()
            self.assertEqual(broker.until_observation("tibber_pulse.reading")["data"], {"power_w": 42})
            self.assertEqual(service.get_state("default", "l1_voltage_v"), voltage)
            self.assertNotEqual(service.get_state("default", "power_w")["observed_at"], sample["observed_at"])
            emitted = []
            try:
                while True:
                    emitted.append(broker.sent.get_nowait())
            except Empty:
                pass
            self.assertFalse(any(p["type"] == "observation.submitted" for p in emitted))

    @skipUnless(os.getenv("CHAINS_TEST_ENGINE_BIN"), "requires a built Go engine")
    def test_reading_reaches_rule_and_authority(self):
        targets = [(key, os.getenv(f"CHAINS_TEST_{key}")) for key in ("MQTT", "REDIS")]
        if not any(uri for _, uri in targets):
            self.skipTest("requires CHAINS_TEST_MQTT or CHAINS_TEST_REDIS")
        for kind, uri in targets:
            if not uri:
                continue
            with self.subTest(bus=kind), TemporaryDirectory() as directory:
                root = Path(directory)
                namespace = "test-tibber-" + uuid4().hex
                rule = root / "meter.chains"
                rule.write_text('Event(type="tibber_pulse.reading", power_w=-60)\n')
                config = root / "config.json"
                config.write_text("{}")
                process = Popen([os.environ["CHAINS_TEST_ENGINE_BIN"], "engine", "-config", str(config),
                                 "-rules", str(rule),
                                 "-database", ":memory:", "-namespace", namespace, "-bus", uri,
                                 "-operator-trace"], stdout=DEVNULL)
                watch = None
                try:
                    watch = TibberPulse.open_transport(ServiceConfig(uri=uri, namespace=namespace), ())

                    def until(check):
                        deadline = monotonic() + 9
                        while monotonic() < deadline:
                            delivery = watch.receive(.2)
                            if delivery and not delivery.reset:
                                value = decode(delivery.data)
                                if check(value):
                                    return value
                        self.fail(f"No matching packet on {kind}")

                    until(lambda item: item["type"] == "state.head")
                    with bridge_server(lambda _path, _auth: (*OK, power(1, -60))) as address, TibberPulse(
                        uri=uri, namespace=namespace, service_id="fixture-meter", poll_interval=0,
                        settings={"bridge_url": address, "password": "fixture"},
                    ) as service:
                        until(lambda item: item["type"] == "state.committed" and item["data"].get("service", {}).get("uuid") == "fixture-meter")
                        service.onPoll()
                        flags = set()

                        def observed(item):
                            info = item["data"]
                            if item["type"] == "operator.trace" and info.get("type") == "rule.finished":
                                flags.add("rule")
                            if (item["type"] == "state.committed" and
                                    "fixture-meter/default" in info.get("devices", {})
                                    and info["devices"]["fixture-meter/default"]["power_w"]["value"] == -60):
                                flags.add("state")
                            return flags == {"rule", "state"}

                        until(observed)
                finally:
                    if watch is not None:
                        watch.close()
                    process.terminate()
                    process.wait(timeout=5)
