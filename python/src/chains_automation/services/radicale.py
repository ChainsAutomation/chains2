"""Radicale synchronization, write actions, and durable event delivery."""
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import json
import logging
import math
from pathlib import Path
import sqlite3
from threading import Event, Lock, Thread
from urllib.parse import urlsplit
from uuid import uuid4
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from icalendar import Alarm, Calendar, Event as CalendarEntry, Todo
import vobject

from ..client import IDENTIFIER
from ..description import capability
from ..service import Service, utcnow
from ..service_config import xdg
from ._dav import DAVClient, DAVError
from ._radicale_items import EVENT_NAMES, alarm_candidates, digest, inspect, parse_date


log = logging.getLogger(__name__)
GRACE = timedelta(minutes=5)
LOOKAHEAD = timedelta(hours=1)
SCHEMA = """
CREATE TABLE IF NOT EXISTS collections (
    alias TEXT PRIMARY KEY, binding TEXT NOT NULL, token TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
    alias TEXT NOT NULL, href TEXT NOT NULL, etag TEXT NOT NULL,
    kind TEXT NOT NULL, payload TEXT NOT NULL, calendar BLOB,
    PRIMARY KEY(alias, href)
);
CREATE TABLE IF NOT EXISTS writes (
    alias TEXT NOT NULL, href TEXT NOT NULL, etag TEXT, change TEXT NOT NULL, cause TEXT NOT NULL,
    PRIMARY KEY(alias, href)
);
CREATE TABLE IF NOT EXISTS outbox (
    id TEXT PRIMARY KEY, alias TEXT NOT NULL, href TEXT NOT NULL, envelope TEXT NOT NULL, expires TEXT
);
CREATE TABLE IF NOT EXISTS fired (id TEXT PRIMARY KEY, expires TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS created_actions (id TEXT PRIMARY KEY, alias TEXT NOT NULL, binding TEXT NOT NULL);
"""


@dataclass
class Settings:
    radicale_url: str = ""
    username: str = ""
    password: str = ""
    collections: dict[str, str] = field(default_factory=dict)
    writable_collections: list[str] = field(default_factory=list)
    deletable_collections: list[str] = field(default_factory=list)
    sync_interval: float = 30
    timezone: str = "UTC"


def declare_event(kind, change):
    method = f"event_{kind}_{change}"
    if kind == "contact":
        def event(self, collection: str, uid: str, name: str):
            self._send_event(_device=collection, _values=self._last_values(kind, change, locals()))
    elif kind == "todo":
        def event(self, collection: str, uid: str, title: str | None, due: str | None, status: str | None):
            self._send_event(_device=collection, _values=self._last_values(kind, change, locals()))
    else:
        def event(self, collection: str, uid: str, title: str | None, start: str | None,
                  end: str | None, status: str | None):
            self._send_event(_device=collection, _values=self._last_values(kind, change, locals()))
    event.__name__ = method
    return capability(name=f"{EVENT_NAMES[kind]}.{change}")(event)


class Radicale(Service):
    settings_type = Settings

    event_entry_created = declare_event("entry", "created")
    event_entry_updated = declare_event("entry", "updated")
    event_entry_deleted = declare_event("entry", "deleted")
    event_todo_created = declare_event("todo", "created")
    event_todo_updated = declare_event("todo", "updated")
    event_todo_deleted = declare_event("todo", "deleted")
    event_contact_created = declare_event("contact", "created")
    event_contact_updated = declare_event("contact", "updated")
    event_contact_deleted = declare_event("contact", "deleted")

    @capability(name="calendar.alarm.due")
    def event_alarm(self, collection: str, uid: str, title: str | None, alarm_id: str,
                    occurrence_id: str | None, scheduled_at: str, late: bool):
        self._send_event(_device=collection, _values=self._last_values("alarm", "due", locals()))

    def describe_devices(self):
        return super().describe_devices() + [
            {"id": alias, "name": alias, "class": "dav_collection", "attributes": ["last_entry", "last_contact", "last_todo", "last_alarm"]}
            for alias in self.settings.collections
        ]

    def onInit(self):
        settings = self.settings
        if not math.isfinite(settings.sync_interval) or settings.sync_interval <= 0:
            raise ValueError("Radicale sync_interval must be finite and positive")
        if not settings.collections:
            raise ValueError("Configure at least one Radicale collection")
        if not (set(settings.writable_collections) | set(settings.deletable_collections)) <= settings.collections.keys():
            raise ValueError("Radicale permissions must name configured collections")
        try:
            self.zone = ZoneInfo(settings.timezone)
        except ZoneInfoNotFoundError:
            raise ValueError("Radicale timezone must be an IANA timezone") from None
        self.dav = DAVClient(settings.radicale_url, settings.username, settings.password)
        self.urls = {}
        for alias, path in settings.collections.items():
            if not IDENTIFIER.fullmatch(alias) or alias in {"system", ".", ".."}:
                raise ValueError("Invalid Radicale collection alias")
            if not path or urlsplit(path).netloc or urlsplit(path).scheme:
                raise ValueError("Radicale collections require DAV paths on the configured server")
            url = self.dav.url(self.dav.base, path.rstrip("/") + "/")
            if url == self.dav.base or url in self.urls.values():
                raise ValueError("Configure each Radicale collection exactly once")
            self.urls[alias] = url
        self.kind = {}
        self.ready_collections = set()
        self.collection_locks = {alias: Lock() for alias in self.urls}
        self.db_lock = Lock()
        self.sync_wake = Event()
        self.delivery_wake = Event()
        self.rebuild_alarms = Event()
        self.workers = []
        root = xdg("XDG_STATE_HOME", ".local/state") / "chains" / "services"
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
        path = root / f"radicale.{self.config.instance}.sqlite"
        path.touch(mode=0o600, exist_ok=True)
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        with self.db:
            self.db.executescript(SCHEMA)
            for row in self.db.execute("SELECT alias,binding FROM collections").fetchall():
                if row["alias"] not in self.urls or row["binding"] != self._binding(row["alias"]):
                    for table in ("collections", "items", "writes", "outbox", "created_actions"):
                        self.db.execute(f"DELETE FROM {table} WHERE alias=?", (row["alias"],))

    def _binding(self, alias):
        return digest(self.urls[alias], self.settings.username)

    def onStart(self):
        for target in (self._sync_loop, self._delivery_loop):
            worker = Thread(target=target, name=f"radicale-{target.__name__}", daemon=True)
            self.workers.append(worker)
            worker.start()

    def onShutdown(self):
        if hasattr(self, "db"):
            self.sync_wake.set()
            self.delivery_wake.set()
            for worker in self.workers:
                worker.join()
            self.db.close()

    def _sync_loop(self):
        while not self.stopping:
            self.sync_wake.clear()
            for alias in self.urls:
                if self.stopping:
                    return
                try:
                    with self.collection_locks[alias]:
                        self.sync_collection(alias)
                except Exception as error:
                    log.warning("Radicale sync failed for %s (%s)", alias, type(error).__name__)
            self.sync_wake.wait(min(self.settings.sync_interval, 86400))

    def sync_collection(self, alias):
        url = self.urls[alias]
        if alias not in self.kind:
            self.kind[alias] = self.dav.resource_kind(url)
        with self.db_lock:
            position = self.db.execute("SELECT token FROM collections WHERE alias=?", (alias,)).fetchone()
            previous = {row["href"]: row for row in self.db.execute("SELECT * FROM items WHERE alias=?", (alias,))}
        listing = self.dav.listing(url, position["token"] if position else "")
        removed = set(previous) - listing.changes.keys() if listing.complete else set()
        changed = {}
        for href, tag in listing.changes.items():
            if tag is None:
                if href in previous:
                    removed.add(href)
            elif href not in previous or previous[href]["etag"] != tag:
                raw, actual_tag = self.dav.get(href)
                if actual_tag != tag:
                    raise ValueError("DAV item changed during synchronization")
                kind, payload = inspect(raw, self.kind[alias])
                changed[href] = (tag, kind, payload, raw if kind == "entry" else None)
        with self.db_lock, self.db:
            for href in sorted(removed):
                row = previous[href]
                if position:
                    self._queue_change(alias, href, "deleted", row["kind"], json.loads(row["payload"]), None)
                self.db.execute("DELETE FROM items WHERE alias=? AND href=?", (alias, href))
                self.db.execute("DELETE FROM outbox WHERE alias=? AND href=? AND expires IS NOT NULL", (alias, href))
            for href, (tag, kind, payload, raw) in changed.items():
                old = previous.get(href)
                if position:
                    if old and (old["kind"] != kind or json.loads(old["payload"])["uid"] != payload["uid"]):
                        self._queue_change(alias, href, "deleted", old["kind"], json.loads(old["payload"]), None)
                        old = None
                    self._queue_change(alias, href, "updated" if old else "created", kind, payload, tag)
                self.db.execute("INSERT OR REPLACE INTO items VALUES (?,?,?,?,?,?)",
                                (alias, href, tag, kind, json.dumps(payload), raw))
                self.db.execute("DELETE FROM outbox WHERE alias=? AND href=? AND expires IS NOT NULL", (alias, href))
            self.db.execute("INSERT OR REPLACE INTO collections VALUES (?,?,?)", (alias, self._binding(alias), listing.token))
            first = alias not in self.ready_collections
            self.ready_collections.add(alias)
        if first or changed or removed:
            self.rebuild_alarms.set()
            self.delivery_wake.set()

    def _queue_change(self, alias, href, change, kind, payload, etag):
        operation = self.db.execute("SELECT * FROM writes WHERE alias=? AND href=? AND change=?",
                                    (alias, href, change)).fetchone()
        cause = None
        if operation:
            if operation["etag"] is None or etag == operation["etag"]:
                cause = json.loads(operation["cause"])
            self.db.execute("DELETE FROM writes WHERE alias=? AND href=?", (alias, href))
        self._enqueue(f"event_{kind}_{change}", alias, href, {"collection": alias, **payload},
                      cause=cause, event_id=uuid4().hex)

    @staticmethod
    def _last_values(kind, change, fields):
        summary = {key: value for key, value in fields.items() if key not in {"self", "kind", "change"}}
        return {"last_" + kind: {**summary, "change": change}}

    def _enqueue(self, method, alias, href, data, *, event_id, cause=None, expires=None):
        kind, _, change = method.removeprefix("event_").partition("_")
        values = self._last_values(kind, change or "due", data)
        envelope = self._prepare_event(method, data, device=alias, values=values, cause=cause, event_id=event_id)
        self.db.execute("INSERT OR IGNORE INTO outbox VALUES (?,?,?,?,?)",
                        (event_id, alias, href, json.dumps(envelope), expires))

    def _alarm_schedule(self, now):
        with self.db_lock:
            rows = [row for row in self.db.execute("SELECT * FROM items WHERE kind='entry'")
                    if row["alias"] in self.ready_collections]
        alarms = []
        for row in rows:
            try:
                candidates = list(alarm_candidates(row["calendar"], row["alias"], now - GRACE, now + LOOKAHEAD, self.zone))
                for due, data in candidates:
                    alarm_id = digest(self._binding(row["alias"]), data["alarm_id"])
                    alarms.append((due, alarm_id, row, {**data, "alarm_id": alarm_id}))
            except Exception as error:
                log.warning("Radicale alarm expansion failed for %s (%s)", row["alias"], type(error).__name__)
        return sorted(alarms, key=lambda item: item[:2])

    def _queue_due(self, alarm, now):
        due, alarm_id, row, data = alarm
        if now - due > GRACE:
            return
        with self.db_lock, self.db:
            item = self.db.execute("SELECT etag FROM items WHERE alias=? AND href=?",
                                   (row["alias"], row["href"])).fetchone()
            if not item or item["etag"] != row["etag"]:
                return
            if self.db.execute("SELECT 1 FROM fired WHERE id=?", (alarm_id,)).fetchone():
                return
            self._enqueue("event_alarm", row["alias"], row["href"],
                          {**data, "late": now - due >= timedelta(seconds=1)},
                          event_id=alarm_id, expires=(due + GRACE).isoformat())

    def _delivery_loop(self):
        agenda = []
        next_refresh = utcnow()
        while not self.stopping:
            self.delivery_wake.clear()
            now = utcnow()
            if self.rebuild_alarms.is_set() or now >= next_refresh:
                self.rebuild_alarms.clear()
                agenda = self._alarm_schedule(now)
                next_refresh = now + LOOKAHEAD / 2
            scheduled = 0
            while scheduled < len(agenda) and agenda[scheduled][0] <= now:
                self._queue_due(agenda[scheduled], now)
                scheduled += 1
            if scheduled:
                del agenda[:scheduled]
            try:
                published = self._deliver(now)
            except Exception as error:
                log.warning("Radicale event delivery failed (%s)", type(error).__name__)
                self.delivery_wake.wait(1)
                continue
            if published:
                continue
            next_due = min(next_refresh, agenda[0][0]) if agenda else next_refresh
            self.delivery_wake.wait(max(0, (next_due - utcnow()).total_seconds()))

    def _deliver(self, now):
        with self.db_lock, self.db:
            stamp = now.isoformat()
            self.db.execute("DELETE FROM fired WHERE expires < ?", (stamp,))
            self.db.execute("DELETE FROM outbox WHERE expires < ?", (stamp,))
            ready = sorted(self.ready_collections)
            filters = ",".join("?" for _ in ready)
            pending = self.db.execute(
                f"SELECT * FROM outbox WHERE expires IS NULL OR alias IN ({filters}) ORDER BY rowid LIMIT 1",
                ready,
            ).fetchone()
        if pending is None:
            return False
        envelope = json.loads(pending["envelope"])
        if envelope.get("type") == "observation.submitted":
            envelope["expires_at"] = (now + timedelta(minutes=1)).isoformat()
            self._publish_observation(envelope)
        else:
            log.warning("Discarding a queued event from the previous wire format")
        with self.db_lock, self.db:
            self.db.execute("DELETE FROM outbox WHERE id=?", (pending["id"],))
            if pending["expires"]:
                self.db.execute("INSERT OR IGNORE INTO fired VALUES (?,?)",
                                (pending["id"], (now + timedelta(days=30)).isoformat()))
        return True

    def _check_write(self, alias, kind, delete):
        allowed = self.settings.deletable_collections if delete else self.settings.writable_collections
        if alias not in allowed:
            raise ValueError("This Radicale collection does not allow the requested write")
        if alias not in self.ready_collections:
            raise ValueError("This Radicale collection has not synchronized")
        if (kind == "contact") != (self.kind[alias] == "contact"):
            raise ValueError("Item type does not match the Radicale collection")

    def _create(self, alias, kind, render):
        self._check_write(alias, kind, False)
        cause = self._action_context()
        uid = digest(self.config.service_id, cause["source"], cause["action_id"]) if cause else uuid4().hex
        contact = kind == "contact"
        href = self.dav.url(self.urls[alias], uid + (".vcf" if contact else ".ics"))
        with self.collection_locks[alias]:
            with self.db_lock:
                receipt = self.db.execute("SELECT alias,binding FROM created_actions WHERE id=?", (uid,)).fetchone()
            if receipt:
                if receipt["alias"] != alias or receipt["binding"] != self._binding(alias):
                    raise ValueError("Radicale action already created an item in another collection")
                return {"collection": alias, "uid": uid}
            try:
                _, headers = self.dav.request("PUT", href, render(uid), **{
                    "If-None-Match": "*", "Content-Type": "text/vcard" if contact else "text/calendar",
                })
                etag = headers.get("ETag")
            except DAVError as error:
                if error.status != 412:
                    raise
                raw, etag = self.dav.get(href)
                found_kind, data = inspect(raw, self.kind[alias])
                if found_kind != kind or data["uid"] != uid:
                    raise ValueError("Radicale creation conflicts with an existing item") from None
            if cause:
                with self.db_lock, self.db:
                    self.db.execute("INSERT OR IGNORE INTO created_actions VALUES (?,?,?)",
                                    (uid, alias, self._binding(alias)))
                    if not self.db.execute("SELECT 1 FROM items WHERE alias=? AND href=?", (alias, href)).fetchone():
                        self._save_write(alias, href, etag, "created", cause)
            self.sync_wake.set()
        return {"collection": alias, "uid": uid}

    def _delete(self, alias, kind, uid):
        self._check_write(alias, kind, True)
        with self.collection_locks[alias]:
            with self.db_lock:
                found = [row for row in self.db.execute("SELECT * FROM items WHERE alias=? AND kind=?", (alias, kind))
                         if json.loads(row["payload"])["uid"] == uid]
            if len(found) != 1:
                raise ValueError("Expected one synchronized Radicale item with that UID")
            item = found[0]
            try:
                self.dav.request("DELETE", item["href"], **{"If-Match": item["etag"]})
            except DAVError as error:
                if error.status == 412:
                    raise ValueError("Radicale item changed since the last sync") from None
                if error.status != 404:
                    raise
                self.sync_wake.set()
            else:
                self._record_write(alias, item["href"], None, "deleted", self._action_context())
        return {"collection": alias, "uid": uid}

    def _record_write(self, alias, href, etag, change, cause):
        if cause:
            with self.db_lock, self.db:
                self._save_write(alias, href, etag, change, cause)
        self.sync_wake.set()

    def _save_write(self, alias, href, etag, change, cause):
        context = {key: cause[key] for key in ("id", "action_id", "correlation_id") if key in cause}
        self.db.execute("INSERT OR REPLACE INTO writes VALUES (?,?,?,?,?)",
                        (alias, href, etag, change, json.dumps(context)))

    @capability(idempotent=True)
    def action_create_calendar_entry(self, collection: str, title: str, start: str, end: str,
                                     reminders_minutes_before: list[int] | None = None):
        first, last = parse_date(start), parse_date(end)
        if type(first) is not type(last) or last <= first:
            raise ValueError("Start and end must use the same date type and end must follow start")
        reminders = reminders_minutes_before or []
        if any(value < 0 or value > 525600 for value in reminders):
            raise ValueError("Reminder offsets must be between zero and 525600 minutes")
        event = CalendarEntry()
        event.add("summary", title)
        event.add("dtstart", first)
        event.add("dtend", last)
        for minutes in sorted(set(reminders)):
            alarm = Alarm()
            alarm.add("action", "DISPLAY")
            alarm.add("description", title)
            alarm.add("trigger", timedelta(minutes=-minutes))
            event.add_component(alarm)
        return self._create(collection, "entry", lambda uid: self._calendar(event, uid))

    @capability(idempotent=True)
    def action_create_todo(self, collection: str, title: str, due: str | None = None):
        todo = Todo()
        todo.add("summary", title)
        todo.add("status", "NEEDS-ACTION")
        if due is not None:
            todo.add("due", parse_date(due))
        return self._create(collection, "todo", lambda uid: self._calendar(todo, uid))

    @capability(idempotent=True)
    def action_create_contact(self, collection: str, name: str, emails: list[str] | None = None,
                              phones: list[str] | None = None):
        def card(uid):
            contact = vobject.vCard()
            contact.add("uid").value = uid
            contact.add("fn").value = name
            contact.add("n").value = vobject.vcard.Name(family=name)
            for address in emails or []:
                contact.add("email").value = address
            for phone in phones or []:
                contact.add("tel").value = phone
            return contact.serialize().encode()
        return self._create(collection, "contact", card)

    @staticmethod
    def _calendar(item, uid):
        calendar = Calendar()
        calendar.add("prodid", "-//Chains Automation//Radicale Service//EN")
        calendar.add("version", "2.0")
        item.add("uid", uid)
        item.add("dtstamp", datetime.now(timezone.utc))
        calendar.add_component(item)
        return calendar.to_ical()

    def action_delete_calendar_entry(self, collection: str, uid: str):
        return self._delete(collection, "entry", uid)

    def action_delete_todo(self, collection: str, uid: str):
        return self._delete(collection, "todo", uid)

    def action_delete_contact(self, collection: str, uid: str):
        return self._delete(collection, "contact", uid)
