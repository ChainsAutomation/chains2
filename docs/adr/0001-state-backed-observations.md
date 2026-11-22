# ADR 0001 — State-backed observations and leaf changes

**Status** Accepted · 2026-11-21

## Context

Chains needs to preserve both what a service observed and the latest known value of each device attribute. With separate event and State publications, a rule could receive a Frigate detection before its corresponding State update, or the publisher could send only the event. Deriving all triggers from State changes would lose repeated observations such as a second identical button press. A service that samples several devices together also needs a way to publish them without exposing a partially updated State tree.

## Decision

- Each live service observation includes a State patch at `service_id/device_id/attribute`. Integrations choose their logical device boundaries and use `system` for service-wide attributes. An occurrence such as a keyboard press can update a `last_*` attribute and provide richer event data. Actions request work. The service reports the resulting device values through observations and carries the action's causation and correlation when it can associate them.
- A submission carries one service, one event type, one schema version, and one or more device patches. The authority validates the whole submission and atomically commits its values and deduplication record in SQLite under one revision. Invalid, stale, or oversized batches are rejected as a unit. Omitted attributes keep their previous values. Explicit JSON `null` is a value.
- The authority publishes the committed State patch and the observation under its original event type. A worker releases the observation to rules once its local view has applied at least that revision. `State(device)` reads the latest known value, which may already be newer. Use `ev.matches` for the exact triggering patches and `ev.observations` for the full batch. Each matching rule trigger or pending wait receives the batch once, even when several device patches match.
- A fresh observation refreshes `observed_at` and can trigger `Event()` even when its value repeats. A first value or a value transition also sets `changed_at` and emits a derived `state.changed` notification for that leaf. Rules opt in with `Event(type="state.changed")`. Startup seeds and State resynchronization apply silently. Registration, heartbeats, action outcomes, and schedules keep their own message semantics.
- Stable IDs identify State paths and action targets. Names are selectors that can match multiple devices. `Devices(...)` returns all matches, and `Device(...)` and name-based actions require a unique target. Rules can iterate over concrete device references to act on several devices explicitly.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| Publish events and State reports independently | Supports payload-only events but leaves ordering and consistency between the two publications to every integration. |
| Trigger rules only from State changes | Simplifies transition rules but loses repeated equal observations and event-specific context. |
| Require action outcomes to return the changed subtree | Couples State to command completion and leaves a second write path for unsolicited device updates, delayed observations, and changes made outside Chains. |
| Publish one event per device in a wide update | Simplifies individual messages but exposes intermediate State and can start several runs for a single sample. |

## Consequences

The authority is the writer and SQLite restores committed State after restart. Workers maintain local read views and can subscribe narrowly by event type. The commit and broker publication are separate operations, so an interruption can leave State committed while a live trigger is missed. Workers recover State from a snapshot and resume live dispatch. Trigger delivery is live-only. [Rules engine concurrency and delivery](../rules-engine-concurrency.md)

Publishers declare the attributes they write, and the authority serializes commits and enforces transport size limits. The change uses wire protocol v2, so engines and publishers require a coordinated upgrade. [Protocol and upgrade guidance](../../README.md#configure-and-extend)
