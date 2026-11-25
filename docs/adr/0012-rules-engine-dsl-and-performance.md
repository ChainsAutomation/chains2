# ADR 0012 — Rules DSL extensions and authority concurrency improvements

**Status** Accepted · 2026-11-25

## Context

Two categories of issues surfaced during a full review of the engine and rules DSL:

1. **Authority lock contention and ingress overhead:** Holding `state.View.mu.Lock()` across frame serialization (`preview`) and SQLite commits stalled all concurrent rule `State()` reads, capability checks, and HTTP queries during disk writes. SQLite ran with `synchronous=FULL` and re-prepared statements per attribute, derived frames were encoded twice, `Runtime.Dispatch` performed a JSON round-trip per event and cloned maps for every rule/patch under `r.mu`, snapshot responses serialized and published inline on the ingress loop, and household dashboard polls contended on the authority commit mutex.
2. **DSL expressiveness and safety gaps:** Common home-automation patterns such as retriggerable motion timers were awkward because `rule.concurrency = 1` dropped re-triggers and secondary `Event()` waits had no per-wait timeout. Rules could read attribute timestamps (`changed_at`, `observed_at`) and device lists (`Devices(...)`, `ev.tags`) but lacked `in`/`not in`, `Len()`, `Now()`, `ParseTime()`, string ordering, and basic numeric helpers (`Min`, `Max`, `Abs`, `Round`). Observation patches also exposed `[]string` metadata slices that failed `for`, indexing, and equality checks, `namedDeviceRef` did not resolve in `==`/`switch`, and bareword selectors could silently capture a `DeviceRef` variable.

## Decision

- **Narrow read/write lock scope and tune SQLite:** `Authority.Observe`, `Register`, and `EditPlacement` serialize writers with `a.mu`, build candidate updates under `View.mu.RLock()`, run `preview` and `db.commit` without holding `View.mu`, and take `View.mu.Lock()` only for the in-memory `apply`. SQLite uses WAL with `PRAGMA synchronous=NORMAL`, `busy_timeout=5000`, cached prepared statements, and guarded rollbacks. `View` maintains an $O(1)$ device index and `Authority` caches the dashboard catalog in memory with `ETag` (`If-None-Match`) support on `GET /household/v1/dashboard`.
- **Reduce hot-path serialization and ingress stalls:** `commitObservation` reuses the wire bytes produced during its size check when publishing derived frames. `Message.Fields()` builds the field map directly without a JSON round-trip. `Runtime.Dispatch` short-circuits when no triggers or waits can match, resolves named device aliases outside `r.mu`, and matches patches without cloning maps on mismatches. Snapshot responses run asynchronously with bounded concurrency off the ingress loop. Redis Streams publishes via `EVALSHA` (with `EVAL` fallback) and Python's `RedisStreams.receive` buffers multi-stream batches with watermark ordering matching Go.
- **Extend the rules DSL:**
  - Add `rule.mode = "drop" | "restart"` (`"restart"` cancels active runs on a new opening match and cannot be combined with `rule.concurrency > 1`).
  - Add per-wait `Event(..., timeout=seconds)` for non-opening waits, returning `null` when the wait expires before the overall rule deadline.
  - Add `in` / `not in` (list, object key, substring), string `<`/`<=`/`>`/`>=`, and built-ins `Len`, `Now`, `ParseTime`, `Min`, `Max`, `Abs`, and `Round`.
  - Normalize observation metadata lists to `[]any` (and accept `[]string` defensively), resolve `namedDeviceRef` during `==`/`!=`/`switch`, validate that string-only event selectors receive string values, and align bareword selectors in `Devices(...)` with `Event(...)`.
  - Preserve outer variables across `for` loops, account for returning branches and exhaustive `switch` statements in static validation, and preserve `elseif` chains in `rules.Format`.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| Keep `View.mu.Lock()` across SQLite commits for simplicity | Guarantees a single lock acquisition but stalls every concurrent rule read and HTTP request during every disk write. |
| Introduce a separate persistent timer service for motion debouncing | Survives restarts, at the cost of persistent run state and coordination; `rule.mode = "restart"` and `Event(..., timeout=...)` solve live debouncing cleanly within the existing live-run model. |
| Remove bareword shorthand from `Event()` and `Device()` entirely | Eliminates all variable/literal ambiguity but breaks existing `.chains` rules and examples. Validating selector types catches accidental capture (such as a `DeviceRef`) without breaking valid rules. |
| Require external clients to track state revisions via SSE only | Removes polling entirely for the household app, but adding `ETag`/`If-None-Match` and in-memory catalog caching immediately eliminates authority contention while staying backward-compatible. |

## Consequences

Authority read latency is decoupled from SQLite commit I/O and snapshot transfers, and rule dispatch avoids unnecessary decoding and map allocations. Rules can express retriggerable timers, bounded waits, list/tag membership, and timestamp age checks directly in the DSL. Rules with `rule.mode = "restart"` remain live-only across restarts, consistent with ADR 0001. [Rules engine concurrency and latency](../rules-engine-concurrency.md)
