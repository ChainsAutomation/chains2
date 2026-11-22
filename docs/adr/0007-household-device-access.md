# ADR 0007 — Household access to registered devices

**Status** Accepted. The dashboard editing permission is superseded by [ADR 0008](0008-household-dashboard-editing.md).

## Context

The household app required a separate sharing grant for each device. This left a fresh installation's household device catalog empty even when the authority had discovered devices.

## Decision

The household token gives access to registered device data and declared attribute setters. Every device appears in All Devices as soon as it registers. The internal operator pseudo-device stays out of the household catalog. Dashboard editing and operator tools continue to require the operator token.

## Alternatives considered

Per-device grants let operators choose which devices to expose, at the cost of a separate onboarding and permission step for every device.

## Consequences

Household users can read device attributes and operate available typed controls, including on devices added later. The `household` tag has ordinary tag semantics. Saved dashboard definitions still use their explicit device references and filters. [Household dashboards](../household-dashboards.md)
