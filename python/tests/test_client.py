from pathlib import Path
import importlib.util
import unittest

from chains_automation import Client, decode, encode, route


FIXTURES = Path(__file__).resolve().parents[2] / "testdata"


class Capture:
    def __init__(self):
        self.sent = []

    def publish(self, topic, payload):
        self.sent.append((topic, decode(payload)))


class ClientTest(unittest.TestCase):
    def test_fixture(self):
        expected = decode((FIXTURES / "event.json").read_bytes())
        self.assertEqual(decode(encode(expected)), expected)
        self.assertEqual(route(expected), "msg/humidity.reported/sensor/_")

    @unittest.skipUnless(importlib.util.find_spec("cbor2"), "CBOR extra unavailable")
    def test_go_cbor_fixture(self):
        expected = decode((FIXTURES / "event.json").read_bytes())
        self.assertEqual(decode((FIXTURES / "event.cbor").read_bytes()), expected)
        self.assertEqual(decode(encode(expected, "cbor")), expected)

    def test_causal_action_report(self):
        transport = Capture()
        client = Client(transport, "fan", "sim_fan")
        request = {"source": "engine", "id": "a", "action_id": "a", "device_id": "fan"}
        client.report("fan", {"power": True}, cause=request)
        client.outcome(request, "completed")
        report, result = [value for _, value in transport.sent]
        self.assertEqual(report["action_id"], "a")
        self.assertEqual(report["causation_id"], "a")
        self.assertEqual(result["target"], "engine")
        self.assertEqual(result["data"]["status"], "completed")
