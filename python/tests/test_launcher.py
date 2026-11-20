import os
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Event, Thread
from time import monotonic, sleep
from types import ModuleType
from unittest import TestCase
from unittest.mock import patch

from chains_automation import Service, message
from chains_automation.__main__ import main
from test_service import Broker


class LauncherTest(TestCase):
    def test_emit_and_config_precedence(self):
        opened = []
        brokers = []
        hooks = []

        class Ping(Service):
            def open_transport(self, config, types):
                opened.append((config.uri, config.poll_interval))
                transport = Broker()
                brokers.append(transport)
                return transport

            def onStart(self):
                hooks.append("start")

            def onPoll(self):
                hooks.append("poll")

            def event_ping(self, tag="default"):
                self._send_event()

            def onShutdown(self):
                hooks.append("shutdown")

        module = ModuleType("fixture_service")
        module.Ping = Ping
        with TemporaryDirectory() as root, patch.dict(os.environ, {
            "XDG_CONFIG_HOME": root, "XDG_STATE_HOME": root, "XDG_CONFIG_DIRS": root,
        }, clear=True), patch.dict("sys.modules", {"fixture_service": module}):
            file = Path(root) / "chains/services/ping.toml"
            file.parent.mkdir(parents=True)
            file.write_text('[service]\nuri = "mqtts://from-file"\npoll_interval = 12\n')
            args = ["fixture_service:Ping", "--emit", "ping"]
            main(args)
            cli = ["--bus", "mqtts://from-cli", "--poll-interval", "2.5"]
            main(args + cli)
            with patch.dict(os.environ, {"CHAINS_BUS": "mqtts://from-env", "CHAINS_POLL_INTERVAL": "0"}):
                main(args + cli)

        self.assertEqual(opened, [
            ("mqtts://from-file", 12), ("mqtts://from-cli", 2.5), ("mqtts://from-env", 0),
        ])
        self.assertEqual(hooks, ["start", "shutdown"] * 3)
        for broker in brokers:
            sent = list(broker.sent.queue)
            self.assertEqual([packet["type"] for packet in sent],
                             ["service.registered", "service.heartbeat", "observation.submitted"])
            self.assertEqual(sent[-1]["data"]["observations"][0]["values"], {"last_ping": {"tag": "default"}})
            self.assertTrue(broker.closed)

    def test_callback_only_service_runs_through_launcher(self):
        broker = Broker()

        class Push(Service):
            def open_transport(self, config, types):
                return broker

            def onStart(self):
                def hardware_callback():
                    self.event_reading(21)
                    self.stop()
                self.hardware = Thread(target=hardware_callback)
                self.hardware.start()

            def event_reading(self, value: int):
                self._send_event()

            def onShutdown(self):
                self.hardware.join()

        module = ModuleType("fixture_push")
        module.Push = Push
        with patch.dict("sys.modules", {"fixture_push": module}), patch.dict(os.environ, {}, clear=True):
            main(["fixture_push:Push", "--service-id", "push"])
        self.assertEqual(broker.until_observation("reading")["data"], {"value": 21})
        self.assertTrue(broker.closed)


class PollTest(TestCase):
    def test_poll_order_retry_and_fixed_delay(self):
        started = Event()
        finished = Event()
        times = []
        broker = Broker()

        class Poll(Service):
            def onStart(self):
                started.set()

            def onPoll(self):
                if not started.is_set():
                    raise AssertionError("polled before onStart finished")
                if not times:
                    sleep(0.08)
                    times.append(monotonic())
                    raise RuntimeError("sample read failed")
                times.append(monotonic())
                finished.set()
                self.stop()

        with TemporaryDirectory() as root:
            file = Path(root) / "poller.toml"
            file.write_text("[service]\npoll_interval = 0.05\n", encoding="utf-8")
            with self.assertLogs("chains_automation.service", level="ERROR"):
                with Poll(transport=broker, service_id="poller", config_path=file):
                    self.assertTrue(finished.wait(2))
        self.assertEqual(broker.sent.get().get("type"), "service.registered")
        self.assertEqual(len(times), 2)
        self.assertGreaterEqual(times[1] - times[0], 0.045)

    def test_slow_poll_leaves_action_and_heartbeat_responsive(self):
        entered, release, stopped = Event(), Event(), Event()
        finished = Event()
        timeline = []
        broker = Broker()

        class Poll(Service):
            def onPoll(self):
                timeline.append("poll")
                entered.set()
                release.wait(5)
                timeline.append("finished")
                finished.set()

            def action_read(self):
                return 7

            def onShutdown(self):
                timeline.append("shutdown")

        service = Poll(transport=broker, service_id="poller", poll_interval=0.01, heartbeat_interval=1)
        service.__enter__()

        def close():
            service.close()
            stopped.set()

        closer = Thread(target=close)
        try:
            self.assertTrue(entered.wait(2))
            broker.until("service.heartbeat")
            broker.inject(message("action.requested", "engine", {"name": "read", "args": {}},
                                  target="poller", device_id="system", schema_version="1", action_id="test"))
            outcome = broker.until("action.outcome", lambda m: m["data"]["status"] == "completed")
            self.assertEqual(outcome["data"]["value"], 7)
            broker.until("service.heartbeat")
            self.assertEqual(timeline, ["poll"])
            closer.start()
            self.assertFalse(stopped.wait(0.05))
        finally:
            release.set()
            if closer.ident is not None:
                closer.join(3)
            else:
                service.close()
        self.assertTrue(finished.is_set())
        self.assertEqual(timeline, ["poll", "finished", "shutdown"])

    def test_zero_disables_polling_and_shutdown_wakes_sleep(self):
        enters = Event()

        class Poll(Service):
            def onPoll(self):
                enters.set()

        with Poll(transport=Broker(), service_id="poller", poll_interval=0):
            self.assertFalse(enters.wait(0.05))
        with Poll(transport=Broker(), service_id="poller", poll_interval=60):
            self.assertTrue(enters.wait(2))
            end = monotonic()
        self.assertLess(monotonic() - end, 2)
