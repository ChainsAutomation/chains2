# ADR 0006 — Separate household and operator web apps

**Status** Accepted. The per-device sharing decision is superseded by [ADR 0007](0007-household-device-access.md).

## Context

The dedicated household experience needs its own front page, navigation, and access. Operators also need a separate place to manage rules, inspect activity, and troubleshoot devices.

## Decision

Build two Solid 2 entry points in the same Go image. The household app starts at `/` and the operator console starts at `/ui/`. Each can be enabled independently on the authority. The apps share device components, typed controls, and the authority's dashboard and State storage.

Use separate scoped browser sessions for `/household/v1` and `/operator/v1`. An operator can sign into the household app to manage dashboards and device sharing. The household API checks permissions independently of which page is open.

## Alternatives considered

A single role-sensitive shell shared navigation and made the household experience part of the operator console. A separate hostname or server could isolate browser origins but would require more deployment and routing configuration.

## Consequences

One hostname and one Go image serve both apps. Existing dashboard IDs, layouts, and sharing grants carry over. The same-origin deployment separates sessions and product surfaces while continuing to trust the web code served by the authority.
