# ADR 0013 — `select:` waits, `when` guards, per-device scoping, and time helpers

**Status** Accepted · 2026-11-25

## Context

Even with `rule.mode = "restart"` and `Event(..., timeout=...)` (ADR 0012), several common home-automation patterns remained awkward or impossible to write cleanly in a single `.chains` rule:

1. **Multi-pattern triggers and racing waits:** Sharing one restartable timer across multiple sensors (such as a hallway door contact and motion sensor) or racing two mid-rule events against a timeout (`occupancy=false` vs. manual switch off vs. 10-minute fallback) could not be expressed without splitting rules or polling.
2. **Pre-admission filtering:** Checking thresholds or device state inside the rule body (`if ev.data.value < 70: return`) happened *after* run admission, allowing non-matching events to cancel active `rule.mode = "restart"` timers and emit noisy traces.
3. **Per-device concurrency/restarts:** `rule.concurrency` and `rule.mode = "restart"` were global to the rule, forcing operators to duplicate one `.chains` file per room instead of writing a single class-based rule with independent per-device timers.
4. **Local clock and duration ergonomics:** `Now()` returned UTC Unix seconds with no local hour/weekday inspection for day/night automations, and `rule.timeout`, `Sleep()`, and `Event(..., timeout=...)` only accepted raw seconds while `Every()` used duration strings.

## Decision

- **`select:` statement (one trigger per `case`):** Works both as an opening trigger (`Event()`, `Every()`, `Time()`) and as a mid-rule wait (also allowing `Sleep()` as a race timeout arm). Each `case` arm takes one wait expression, an optional binding (`case ev = Event(...):`), an optional `when` guard, and its own branch body. All arms of an opening `select:` share one rule concurrency/restart group, and the first matching arm in source order wins deterministically per observation batch. Mid-rule `select:` arms race on a shared winner channel and immediately unregister losing waits and timers.
- **Pre-admission `when <expr>` guards:** Optional on standalone `Event()` statements and `Event()` arms in `select:`. Guards may use pure expressions plus read-only `State()`, `Devices()`, `LocalTime()`, and `Now()`, and are evaluated **outside** `Runtime.mu` before admitting, restarting, or waking a run.
- **Run scoping (`rule.scope`):** Accepts `"rule"` (default), `"device"` (independent concurrency/restart per triggering `ev.device`), or `"service"` (per `service_id`). Exposed alongside `rule.mode` in the guided web rule form.
- **`LocalTime()` and duration strings:** `LocalTime()` returns `{year, month, day, weekday, hour, minute, second}` in the engine's configured `Calendar.Zone`. `rule.timeout`, `Sleep()`, and `Event(..., timeout=...)` accept Go duration strings (`"15m"`, `"500ms"`) in addition to numeric seconds, and `Duration(str)` converts a duration string to seconds for timestamp arithmetic.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| `Event(...) or Event(...)` expression | Overloads short-circuit boolean `or` with parallel wait registration, cannot branch per matched pattern without duplicate `if` checks, and mixes awkwardly with `Time()` / `Sleep()`. |
| Comma-separated triggers within one `select:` `case` arm | Saves a few lines when two arms share an identical body, at the cost of more complex binding/guard/formatting rules; keeping one trigger per `case` keeps `select:` uniform and lets arms fall through into shared code after `select:`. |
| Restricting `when` guards to only `ev` fields (no `State()`/`LocalTime()`) | Avoids host reads during dispatch, but prevents common guards like `when State(fan).power == false`; evaluating guards outside `Runtime.mu` keeps lock contention unaffected. |
| Persistent rule-local global variables across runs | Useful for counters, but hides state outside the observable State tree and violates ADR 0001. |

## Consequences

Rules can express multi-sensor triggers, event-vs-timeout races, night-mode adaptive dimming, and single-rule multi-room restartable timers cleanly in native DSL. Because pattern matching and `when` guards run outside `Runtime.mu`, read-only `State()` checks in guards do not contend on the dispatcher lock. Existing `.chains` rules remain 100% compatible. [Writing Rules](../rules.md) · [Rules engine concurrency and latency](../rules-engine-concurrency.md)
