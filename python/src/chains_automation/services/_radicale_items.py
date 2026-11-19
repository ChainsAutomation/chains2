"""DAV object normalization and iCalendar alarm expansion."""
from datetime import date, datetime, timedelta, timezone
from hashlib import sha256
import json
import re

from icalendar import Calendar
import recurring_ical_events
import vobject


KINDS = {"VEVENT": "entry", "VTODO": "todo"}
EVENT_NAMES = {"entry": "calendar.entry", "todo": "calendar.todo", "contact": "contact"}
ZERO = timedelta(0)
TIMESTAMP = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})")


def digest(*parts):
    return sha256(json.dumps(parts, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def text(component, key):
    value = component.get(key)
    return str(value)[:4096] if value is not None else None


def iso(value):
    return value.isoformat() if value is not None else None


def decoded(component, key):
    return component.decoded(key, None)


def inspect(raw, collection_kind):
    if collection_kind == "contact":
        card = vobject.readOne(raw.decode("utf-8"))
        if card.name != "VCARD" or not getattr(card, "uid", None) or not card.uid.value:
            raise ValueError("Contact is missing a UID")
        result = {"uid": str(card.uid.value), "name": str(card.fn.value)[:4096]}
        return "contact", result

    calendar = Calendar.from_ical(raw)
    components = [item for item in calendar.subcomponents if item.name in KINDS]
    if not components:
        raise ValueError("Calendar object is missing a VEVENT or VTODO")
    item = next((item for item in components if "RECURRENCE-ID" not in item), components[0])
    result = {"uid": str(item["UID"]), "title": text(item, "SUMMARY"),
              "status": text(item, "STATUS")}
    if not result["uid"]:
        raise ValueError("Calendar item is missing a UID")
    if item.name == "VEVENT":
        start = decoded(item, "DTSTART")
        result.update(start=iso(start), end=iso(occurrence_end(item, start)))
    else:
        result["due"] = iso(decoded(item, "DUE"))
    return KINDS[item.name], result


def as_time(value, zone):
    if isinstance(value, datetime):
        return value.replace(tzinfo=zone) if value.tzinfo is None else value
    if isinstance(value, date):
        return datetime.combine(value, datetime.min.time(), zone)
    return None


def parse_date(value):
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}", value):
        return date.fromisoformat(value)
    if not TIMESTAMP.fullmatch(value):
        raise ValueError("Use a date or an RFC3339 timestamp with an offset")
    return datetime.fromisoformat(value).astimezone(timezone.utc)


def occurrence_end(item, start):
    end = decoded(item, "DTEND")
    if end is not None or start is None:
        return end
    duration = decoded(item, "DURATION")
    if duration is None:
        duration = ZERO if isinstance(start, datetime) else timedelta(days=1)
    return add_duration(start, duration) if isinstance(start, datetime) else start + duration


def add_duration(base, duration):
    """Account for timezone transitions before applying the clock-time part."""
    if base.tzinfo is None:
        return base + duration
    sign = -1 if duration < ZERO else 1
    days = timedelta(days=abs(duration).days)
    wall_time = base + sign * days if days else base
    return (wall_time.astimezone(timezone.utc) + sign * (abs(duration) - days)).astimezone(base.tzinfo)


def alarm_specs(item):
    counts = {}
    for alarm in item.subcomponents:
        if alarm.name != "VALARM":
            continue
        trigger = alarm.get("TRIGGER")
        if trigger is None or not isinstance(trigger.dt, (datetime, timedelta)):
            continue
        relative_end = str(trigger.params.get("RELATED", "")).upper() == "END"
        repeat = int(alarm.get("REPEAT", 0))
        duration = decoded(alarm, "DURATION")
        if repeat < 0 or repeat > 1000 or (repeat and (not isinstance(duration, timedelta) or duration <= ZERO)):
            continue
        duration = duration if repeat else ZERO
        identity = text(alarm, "UID") or digest(str(trigger.dt), relative_end, repeat, str(duration),
                                                text(alarm, "ACTION"))
        ordinal = counts.get(identity, 0)
        counts[identity] = ordinal + 1
        yield (identity, ordinal), trigger.dt, relative_end, repeat, duration


def alarm_candidates(raw, collection, lower, upper, zone):
    calendar = Calendar.from_ical(raw)
    entries = calendar.walk("VEVENT")
    offsets = []
    seen = set()

    def expand(item, spec, first, occurrence):
        identity, _, _, repeat, duration = spec
        scheduled = first
        for index in range(repeat + 1):
            if index:
                scheduled = add_duration(scheduled, duration)
            due = scheduled.astimezone(timezone.utc)
            if not lower <= due <= upper:
                continue
            uid = str(item["UID"])
            alarm_id = digest(collection, uid, occurrence, identity, iso(due))
            if alarm_id in seen:
                continue
            seen.add(alarm_id)
            yield due, {"alarm_id": alarm_id, "collection": collection, "uid": uid,
                        "title": text(item, "SUMMARY"), "occurrence_id": occurrence,
                        "scheduled_at": iso(due)}

    for item in entries:
        if text(item, "STATUS") == "CANCELLED":
            continue
        for spec in alarm_specs(item):
            _, trigger, _, repeat, duration = spec
            if isinstance(trigger, datetime):
                yield from expand(item, spec, as_time(trigger, zone), iso(decoded(item, "RECURRENCE-ID")))
            else:
                offsets.extend((trigger, trigger + repeat * duration))
    if not offsets:
        return

    # Floating and all-day times are evaluated in the configured timezone.
    earliest = (lower - max(offsets) - timedelta(days=1)).astimezone(zone)
    latest = (upper - min(offsets) + timedelta(days=1)).astimezone(zone)
    for item in recurring_ical_events.of(calendar).between(earliest, latest):
        if text(item, "STATUS") == "CANCELLED":
            continue
        start = decoded(item, "DTSTART")
        occurrence = iso(decoded(item, "RECURRENCE-ID") or start)
        for spec in alarm_specs(item):
            _, trigger, relative_end, _, _ = spec
            if not isinstance(trigger, timedelta):
                continue
            base = as_time(occurrence_end(item, start) if relative_end else start, zone)
            if base is not None:
                yield from expand(item, spec, add_duration(base, trigger), occurrence)
