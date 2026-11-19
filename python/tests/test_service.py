import os
from pathlib import Path
from queue import Empty, Queue
from tempfile import TemporaryDirectory
from threading import Event
from unittest import TestCase
from unittest.mock import patch

from chains_automation import Delivery, Service, capability, decode, encode, message, route


class Broker:
    def __init__(self):
        self.incoming = Queue()
        self.sent = Queue()
        self.closed = False

    def publish(self, topic, raw):
        packet = decode(raw)
        assert route(packet) == topic
        self.sent.put(packet)
        if packet["type"] == "state.query.requested":
            self.inject(message(
                "state.query.responded", "engine",
                {"found": True, "state": {"value": 42}},
                target=packet["source"], causation_id=packet["id"],
            ))

    def inject(self, packet):
        self.incoming.put(Delivery(route(packet), encode(packet)))

    def receive(self, timeout):
        try:
            return self.incoming.get(timeout=timeout)
        except Empty:
            return None

    def until(self, kind, predicate=lambda _: True):
        while True:
            packet = self.sent.get(timeout=2)
            if packet["type"] == kind and predicate(packet):
                return packet

    def close(self):
        self.closed = True


class Demo(Service):
    attributes = ("power", "speed")

    def onInit(self):
        self.subscribe("*")
        self.seed_state(power=False)
        self.observed = Event()
        self.reconnected = Event()

    def event_ping(self):
        self._send_event()

    @capability(name="temperature.reported", version="2")
    def event_temperature(self, celsius: float, valid: bool = True):
        self._send_event()

    def action_read(self, attribute: str):
        return self.query_state("other", "sensor", attribute)["state"]["value"]

    def onEvent(self, event):
        if event["type"] == "other.event":
            self.observed.set()

    def onReconnect(self):
        self.reconnected.set()


class ServiceTest(TestCase):
    def test_reflection_and_lifecycle(self):
        broker = Broker()
        demo = Demo(service_id="demo")
        opened = []

        def factory(config, types):
            opened.append((config.uri, types))
            return broker

        demo.transport_factory = factory
        description = demo.describe()
        self.assertEqual(description["uuid"], "demo")
        schema = description["events"]["temperature.reported"]["2"]
        self.assertEqual(schema["required"], ["celsius"])
        self.assertEqual(schema["properties"]["celsius"], {"type": "number"})
        self.assertIn("get_state", description["devices"][0]["actions"])
        self.assertEqual(opened, [])
        with demo:
            self.assertEqual(opened, [("mqtt://localhost:1883", ())])
            broker.until("service.registered")
            self.assertTrue(broker.until("state.reported")["silent"])
            first = broker.until("service.heartbeat")["data"]["instance_id"]
            demo.event_ping()
            self.assertEqual(broker.until("ping")["data"], {})
            demo.event_temperature(23.0)
            self.assertEqual(broker.until("temperature.reported")["data"], {"celsius": 23.0, "valid": True})
            demo.set_state(power=True, speed=3)
            self.assertTrue(demo.get_state("default", "power")["value"])
            self.assertEqual(broker.until("state.reported")["data"], {"power": True, "speed": 3})
            broker.inject(message("other.event", "other", {}))
            self.assertTrue(demo.observed.wait(2))
            broker.incoming.put(Delivery(reset=True))
            broker.until("service.registered")
            self.assertEqual(broker.until("service.heartbeat")["data"]["instance_id"], first)
            self.assertTrue(broker.until("state.reported")["silent"])
            self.assertTrue(demo.reconnected.wait(2))
        self.assertTrue(broker.closed)

    def test_action_can_query_while_receiver_waits(self):
        broker = Broker()
        with Demo(service_id="demo", transport=broker) as demo:
            def send_action(name, args, target="demo"):
                request = message("action.requested", "engine", {"name": name, "args": args},
                                  device_id="default", target=target, action_id=name, schema_version="1")
                broker.inject(request)
                return request

            send_action("read", {"attribute": "power"}, "somebody-else")
            send_action("read", {"attribute": "power"})
            reply = broker.until("action.outcome", lambda p: p["data"]["status"] == "completed")
            self.assertEqual(reply["data"]["value"], 42)
            request = send_action("get_state", {"device_id": "default", "attribute": "power"})
            reply = broker.until("action.outcome", lambda p: p["action_id"] == "get_state" and p["data"]["status"] == "completed")
            self.assertEqual(reply["causation_id"], request["id"])
            self.assertFalse(reply["data"]["value"]["value"])

    def test_empty_default_and_opt_in_restore(self):
        with TemporaryDirectory() as root, patch.dict(os.environ, {"XDG_STATE_HOME": root}):
            original = Demo()
            self.assertEqual(original.get_state(), {})
            identity = original.describe()["uuid"]
            original.set_state(power=True)
            path = Path(root) / "state.json"
            original.save_state(path)
            restored = Demo()
            self.assertEqual(restored.describe()["uuid"], identity)
            self.assertEqual(restored.get_state(), {})
            restored.load_state(path)
            self.assertEqual(restored.get_state(), original.get_state())
