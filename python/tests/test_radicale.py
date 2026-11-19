from datetime import datetime, timedelta, timezone
from importlib.util import find_spec
import os
from tempfile import TemporaryDirectory
from unittest import TestCase, skipUnless
from unittest.mock import patch
from uuid import uuid4
from zoneinfo import ZoneInfo

from chains_automation import message


EXTRA = bool(find_spec("icalendar") and find_spec("recurring_ical_events") and find_spec("vobject"))
if EXTRA:
    from chains_automation.service import _action
    from chains_automation.services._radicale_items import alarm_candidates
    from chains_automation.services.radicale import Radicale


def raw_calendar(body):
    return ("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Fixture//EN\n" + body + "END:VCALENDAR\n").encode()


def moment(value):
    return datetime.fromisoformat(value).replace(tzinfo=timezone.utc)


@skipUnless(EXTRA, "requires the radicale extra")
class AlarmTest(TestCase):
    def test_recurrence_overrides_cancellation_and_dst(self):
        raw = raw_calendar("""BEGIN:VEVENT
UID:weekly
DTSTART;TZID=America/New_York:20260307T090000
DTEND;TZID=America/New_York:20260307T100000
RRULE:FREQ=DAILY;COUNT=4
EXDATE;TZID=America/New_York:20260309T090000
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Reminder
TRIGGER;RELATED=END:-PT15M
REPEAT:1
DURATION:PT5M
END:VALARM
END:VEVENT
BEGIN:VEVENT
UID:weekly
RECURRENCE-ID;TZID=America/New_York:20260308T090000
DTSTART;TZID=America/New_York:20260308T090000
STATUS:CANCELLED
END:VEVENT
BEGIN:VEVENT
UID:weekly
RECURRENCE-ID;TZID=America/New_York:20260310T090000
DTSTART;TZID=America/New_York:20260310T103000
DTEND;TZID=America/New_York:20260310T113000
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Moved
TRIGGER:-PT15M
END:VALARM
END:VEVENT
""")
        alarms = list(alarm_candidates(raw, "home", moment("2026-03-07"), moment("2026-03-11"),
                                       ZoneInfo("America/New_York")))
        self.assertEqual([alarm[0].isoformat() for alarm in alarms], [
            "2026-03-07T14:45:00+00:00", "2026-03-07T14:50:00+00:00", "2026-03-10T14:15:00+00:00",
        ])
        self.assertIn("T09:00:00", alarms[-1][1]["occurrence_id"])

    def test_clock_offset_crosses_dst_gap(self):
        raw = raw_calendar("""BEGIN:VEVENT
UID:spring
DTSTART;TZID=America/New_York:20260308T031500
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Reminder
TRIGGER:-PT30M
END:VALARM
END:VEVENT
""")
        alarms = list(alarm_candidates(raw, "home", moment("2026-03-08"), moment("2026-03-09"),
                                       ZoneInfo("America/New_York")))
        self.assertEqual([alarm[0].isoformat() for alarm in alarms], ["2026-03-08T06:45:00+00:00"])

    def test_all_day_and_absolute_alarms(self):
        raw = raw_calendar("""BEGIN:VEVENT
UID:all-day
DTSTART;VALUE=DATE:20261025
DTEND;VALUE=DATE:20261026
SUMMARY:All day
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:All day
TRIGGER:-PT15M
END:VALARM
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Absolute
TRIGGER;VALUE=DATE-TIME:20261025T140000Z
END:VALARM
END:VEVENT
""")
        alarms = list(alarm_candidates(raw, "home", moment("2026-10-24"), moment("2026-10-27"),
                                       ZoneInfo("Europe/Oslo")))
        self.assertEqual(sorted(alarm[0].isoformat() for alarm in alarms),
                         ["2026-10-24T21:45:00+00:00", "2026-10-25T14:00:00+00:00"])


@skipUnless(EXTRA and os.getenv("CHAINS_TEST_RADICALE"), "requires the radicale extra and CHAINS_TEST_RADICALE")
class DAVIntegrationTest(TestCase):
    def setUp(self):
        temp = TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        environment = patch.dict(os.environ, XDG_STATE_HOME=temp.name)
        environment.start()
        self.addCleanup(environment.stop)
        prefix = uuid4().hex
        self.settings = {
            "radicale_url": os.environ["CHAINS_TEST_RADICALE"], "username": "fixture", "password": "fixture",
            "collections": {"home": f"fixture/{prefix}-calendar/", "people": f"fixture/{prefix}-people/"},
            "writable_collections": ["home", "people"], "deletable_collections": ["home"],
        }
        self.service = self.open_service()
        s = self.service
        s.dav.request("MKCALENDAR", s.urls["home"])
        s.dav.request("MKCOL", s.urls["people"], b"""<mkcol xmlns="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
          <set><prop><resourcetype><collection/><c:addressbook/></resourcetype></prop></set></mkcol>""",
                      **{"Content-Type": "application/xml"})

    def open_service(self):
        s = Radicale(service_id="fixture", settings=self.settings)
        s.onInit()
        s._devices = {device["id"]: device for device in s.describe()["devices"]}
        self.addCleanup(s.close)
        return s

    @staticmethod
    def records(s):
        import json
        return [json.loads(row["envelope"]) for row in s.db.execute("SELECT * FROM outbox ORDER BY rowid")]

    def test_dav_checkpoint_restart_and_actions(self):
        s = self.service
        home = s.urls["home"]
        existing = s.dav.url(home, "existing.ics")
        item = raw_calendar("""BEGIN:VEVENT
UID:existing
DTSTAMP:20260101T000000Z
DTSTART:20260102T120000Z
SUMMARY:Existing
END:VEVENT
""")
        s.dav.request("PUT", existing, item, **{"Content-Type": "text/calendar"})
        s.sync_collection("home")
        s.sync_collection("people")
        self.assertEqual(self.records(s), [])
        request = message("action.requested", "engine", {}, action_id="example", correlation_id="trigger")
        ctx = _action.set(request)
        try:
            created = s.action_create_calendar_entry("home", "New", "2030-01-02", "2030-01-03", [5])
            self.assertEqual(s.action_create_calendar_entry("home", "New", "2030-01-02", "2030-01-03", [5]), created)
        finally:
            _action.reset(ctx)
        todo = s.action_create_todo("home", "Task", "2030-01-03")
        contact = s.action_create_contact("people", "New Person", ["private@example.test"], ["1234"])
        with self.assertRaisesRegex(ValueError, "does not allow"):
            s.action_delete_contact("people", contact["uid"])
        s.sync_collection("home")
        s.sync_collection("people")
        records = self.records(s)
        self.assertCountEqual([record["type"] for record in records],
                              ["calendar.entry.created", "calendar.todo.created", "contact.created"])
        entry = next(record for record in records if record["type"] == "calendar.entry.created")
        self.assertEqual((entry["causation_id"], entry["correlation_id"], entry["action_id"]),
                         (request["id"], "trigger", "example"))
        person = next(record for record in records if record["type"] == "contact.created")
        self.assertEqual(person["data"], {"collection": "people", "uid": contact["uid"], "name": "New Person"})

        updated = item.replace(b"SUMMARY:Existing", b"SUMMARY:Updated")
        s.dav.request("PUT", existing, updated, **{"Content-Type": "text/calendar"})
        with self.assertRaisesRegex(ValueError, "changed"):
            s.action_delete_calendar_entry("home", "existing")
        with s.db:
            s.db.execute("UPDATE collections SET token=? WHERE alias='home'",
                         ("http://radicale.org/ns/sync/" + "0" * 64,))
        s.close()
        s = self.open_service()
        s.sync_collection("home")
        s.sync_collection("people")
        self.assertEqual([r["id"] for r in self.records(s)[:3]], [r["id"] for r in records])
        self.assertEqual(self.records(s)[-1]["data"]["title"], "Updated")
        s.action_delete_calendar_entry("home", "existing")
        s.action_delete_todo("home", todo["uid"])
        s.sync_collection("home")
        self.assertCountEqual([r["type"] for r in self.records(s)[-2:]], [
            "calendar.entry.deleted", "calendar.todo.deleted",
        ])

    def test_alarm_catchup_and_persistent_deduplication(self):
        s = self.service
        now = moment("2030-01-01T12:05:00")
        for uid, start in (("near", "120000"), ("old", "115959")):
            raw = raw_calendar(f"""BEGIN:VEVENT
UID:{uid}
DTSTAMP:20260101T000000Z
DTSTART:20300101T{start}Z
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Alarm
TRIGGER:PT0M
END:VALARM
END:VEVENT
""")
            href = s.dav.url(s.urls["home"], uid + ".ics")
            s.dav.request("PUT", href, raw, **{"Content-Type": "text/calendar"})
        s.sync_collection("home")
        self.assertEqual(self.records(s), [])
        agenda = s._alarm_schedule(now)
        self.assertEqual([alarm[3]["uid"] for alarm in agenda], ["near"])
        s._queue_due(agenda[0], now)
        self.assertTrue(self.records(s)[0]["data"]["late"])
        with patch.object(s, "_send", side_effect=TimeoutError):
            with self.assertRaises(TimeoutError):
                s._deliver(now)
        original = self.records(s)[0]["id"]
        s.close()
        s = self.open_service()
        with patch.object(s, "_send") as publish:
            self.assertFalse(s._deliver(now))
            publish.assert_not_called()
        s.sync_collection("home")
        with patch.object(s, "_send") as publish:
            self.assertTrue(s._deliver(now))
            self.assertEqual(publish.call_args.args[0]["id"], original)
        s.close()
        s = self.open_service()
        self.assertEqual(s._alarm_schedule(now), [])
        s.sync_collection("home")
        s._queue_due(s._alarm_schedule(now)[0], now)
        self.assertEqual(self.records(s), [])

    @skipUnless(os.getenv("CHAINS_TEST_ENGINE_BIN"), "requires a built Go engine")
    def test_rule_action_round_trip(self):
        from pathlib import Path
        from subprocess import DEVNULL, Popen
        from time import monotonic, sleep

        from chains_automation import decode
        from chains_automation.service import ServiceConfig

        targets = [(kind, os.getenv(f"CHAINS_TEST_{kind}")) for kind in ("MQTT", "REDIS")]
        if not any(uri for _, uri in targets):
            self.skipTest("requires CHAINS_TEST_MQTT or CHAINS_TEST_REDIS")
        for kind, uri in targets:
            if not uri:
                continue
            with self.subTest(bus=kind), TemporaryDirectory() as directory:
                namespace = "test-radicale-" + uuid4().hex
                root = Path(directory)
                (root / "config.json").write_text("{}")
                (root / "mirror.chains").write_text(
                    'Event(type="calendar.entry.created", title="Rule trigger")\n'
                    'Action("create_todo", service_name="Radicale Fixture", device_name="Default", '
                    'collection="home", title="Created by rule")\n'
                )
                engine = Popen([os.environ["CHAINS_TEST_ENGINE_BIN"], "engine", "-config", str(root / "config.json"),
                                "-rules", str(root / "mirror.chains"), "-database", ":memory:",
                                "-namespace", namespace, "-bus", uri], stdout=DEVNULL)
                try:
                    watch = Radicale.open_transport(ServiceConfig(uri=uri, namespace=namespace), ())
                    try:
                        # Repeated registration on reconnect is independent of this fixture.
                        # Wait for the authority's bus heartbeat before starting the service.
                        def next_message(event, predicate=lambda _: True):
                            deadline = monotonic() + 8
                            while monotonic() < deadline:
                                delivery = watch.receive(.2)
                                if delivery and not delivery.reset:
                                    packet = decode(delivery.data)
                                    if packet["type"] == event and predicate(packet):
                                        return packet
                            self.fail(f"Did not receive {event} on {kind}")

                        next_message("state.head")
                        settings = {**self.settings, "sync_interval": .1}
                        with Radicale(uri=uri, namespace=namespace, instance=kind.lower(),
                                      name="Radicale Fixture", service_id="radicale-fixture", settings=settings) as service:
                            next_message("state.committed", lambda p: p["data"].get("service", {}).get("uuid")
                                         == "radicale-fixture")
                            deadline = monotonic() + 5
                            while "home" not in service.ready_collections and monotonic() < deadline:
                                sleep(.02)
                            self.assertIn("home", service.ready_collections)
                            entry_id = uuid4().hex
                            item = raw_calendar(f"""BEGIN:VEVENT
UID:{entry_id}
DTSTAMP:20260101T000000Z
DTSTART:20300101T120000Z
SUMMARY:Rule trigger
END:VEVENT
""")
                            href = service.dav.url(service.urls["home"], entry_id + ".ics")
                            service.dav.request("PUT", href, item, **{"Content-Type": "text/calendar"})
                            trigger = next_message("calendar.entry.created",
                                                   lambda p: p["data"]["uid"] == entry_id)
                            result = next_message("calendar.todo.created",
                                                  lambda p: p["data"]["title"] == "Created by rule")
                            self.assertEqual(result["correlation_id"], trigger["id"])
                            self.assertTrue(result["action_id"])
                    finally:
                        watch.close()
                finally:
                    engine.terminate()
                    engine.wait(timeout=5)
