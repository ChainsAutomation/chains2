from contextlib import redirect_stdout
from importlib.util import find_spec
from io import StringIO
import os
from queue import Empty, Queue
from time import monotonic, sleep
from unittest import TestCase, skipUnless
from unittest.mock import patch

from chains_automation import Delivery
from chains_automation.__main__ import main
from chains_automation.services.frigate import Frigate, parse_update
from chains_automation.services.rflink import RFLink, parse_frame
from test_service import Broker


class BundledServicesTest(TestCase):
    def test_listing(self):
        output = StringIO()
        with redirect_stdout(output):
            main(["--list"])
        self.assertIn("frigate", output.getvalue().splitlines())
        self.assertIn("rflink", output.getvalue().splitlines())
        self.assertIn("radicale", output.getvalue().splitlines())

    def test_rflink_fields_and_signed_measurements(self):
        raw = b"20;A2;UPM/Esic;ID=0001;SWITCH=02;CMD=SET_LEVEL=14;TEMP=80DC;HUM=42;BAT=LOW;EXTRA=ab;\r"
        frame = parse_frame(raw)
        self.assertEqual(frame["protocol"], "UPM/Esic")
        self.assertEqual(frame["rf_id"], "0001")
        self.assertEqual(frame["command"], "SET_LEVEL=14")
        self.assertEqual(frame["fields"]["EXTRA"], "ab")
        self.assertEqual(frame["measurements"], {"temperature_c": -22.0, "humidity_percent": 42, "battery_low": True})
        self.assertEqual(frame["raw"].encode(), raw.rstrip(b"\r"))
        self.assertIsNone(parse_frame(b"20;00;RFLink Gateway booted;"))
        self.assertIsNone(parse_frame(b"10;02;UPM;ID=0001;"))
        self.assertEqual(parse_frame(b"20;00;UPM;ID=0001;TEMP=unknown;")["measurements"], {})

    @skipUnless(find_spec("serial") and os.name == "posix", "requires pySerial and a Unix pseudo-terminal")
    def test_rflink_serial_fragment_and_multiple_frames(self):
        import pty
        import termios

        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)
        broker = Broker()
        with RFLink(service_id="rflink-fixture", transport=broker, settings={"port": os.ttyname(slave)}):

            # PTYs discard writes made before the service opens and configures the port.
            until = monotonic() + 2
            while termios.tcgetattr(slave)[3] & termios.ICANON:
                if monotonic() >= until:
                    self.fail("RFLink serial port did not open")
                sleep(.01)
            os.write(master, b"20;22;Kaku;ID=0005;")
            os.write(master, b"SWITCH=2;CMD=ON;\r\n20;23;Kaku;ID=0005;CMD=OFF;\r\n")
            self.assertEqual(broker.until("rflink.signal")["data"]["command"], "ON")
            self.assertEqual(broker.until("rflink.signal")["data"]["command"], "OFF")

    def test_frigate_parser(self):
        frame = parse_update(b'{"type":"end","before":{"x":1},"after":{"id":"1","camera":"front","label":"person","current_zones":["door"]}}')
        self.assertEqual((frame["phase"], frame["camera"], frame["label"], frame["zones"]),
                         ("end", "front", "person", ["door"]))
        self.assertEqual(frame["payload"]["before"], {"x": 1})
        self.assertIsNone(parse_update(b'{"type":"new","after":null}'))

    def test_frigate_separate_mqtt_source(self):
        source = Queue()
        destination = Broker()
        closed = []

        class Source:
            def __init__(self, address, *, raw_topics):
                self.address = address
                self.topics = raw_topics

            def receive(self, timeout):
                try:
                    return source.get(timeout=timeout)
                except Empty:
                    return None

            def close(self):
                closed.append((self.address, self.topics))

        with patch("chains_automation.services.frigate.MQTT", Source):
            with Frigate("redis://localhost", service_id="frigate-fixture", transport=destination,
                         settings={"mqtt_uri": "mqtt://frigate", "topic_prefix": "home/cameras",
                                   "topics": ["reviews"]}):
                topic = "home/cameras/reviews"
                frame = b'{"type":"new","after":{"id":"r1","camera":"front","severity":"alert","data":{"zones":["yard"]}}}'
                source.put(Delivery(topic, frame, retained=True))
                source.put(Delivery(topic, b'invalid JSON'))
                source.put(Delivery(topic, frame))
                source.put(Delivery(topic, frame.replace(b'"new"', b'"update"')))
                first = destination.until("frigate.review")
                second = destination.until("frigate.review")
                self.assertEqual(first["data"]["zones"], ["yard"])
                self.assertEqual(second["data"]["phase"], "update")
                self.assertEqual(first["data"]["native_id"], second["data"]["native_id"])
                self.assertNotEqual(first["id"], second["id"])
        self.assertEqual(closed, [("mqtt://frigate", ("home/cameras/reviews",))])
