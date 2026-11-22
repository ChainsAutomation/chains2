# ADR 0003 — Device classes, setters, and action outcomes

**Status** Accepted · 2026-11-22

## Context

The web console can derive action inputs from JSON Schema. It also needs to know which State attributes have controls, how to label them, and which devices support the same controls across integrations. Rules need to choose whether to wait for an action outcome.

## Decision

Devices advertise a list of built-in class contracts. A toggle has readable and writable boolean power. Lights inherit that contract, and dimmers also expose an integer percentage. The authority expands inheritance and checks declarations. Other devices can expose capabilities directly through typed attribute descriptions.

Each writable attribute points to a declared action version and its value argument. The console and DSL use the same setter and validation. Named actions remain available for operations with their own arguments or results. Service observations remain the source of reported State.

Actions wait for outcomes by default. An explicit send-only mode returns a receipt once the request enters the transport or the hosted queue. The presence of an assignment has no effect on execution. Correlated State confirmation can be added separately.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| Infer capabilities from labels or setter names | Reduces registration data and leaves portability dependent on naming and conventions. |
| Require every action to be a State patch | Simplifies generic setters and loses distinct operations such as calendar creation and native toggling. |
| Wait only when a rule uses the result | Shortens send-only calls and makes adding an assignment change sequencing. |
| Expose each dimmer's native range | Preserves raw device steps and makes rules dependent on vendor units. |

## Consequences

Integrations declare the types and setters they offer. The Zigbee2MQTT bridge converts brightness between native units and the shared percentage. The wire version moves to v3, and services re-register their descriptions while State values on unchanged paths are retained. [Device controls and actions](../actions.md)
