# ADR 0005 — Dashboard ownership and household access

**Status** Accepted

## Context

A household needs a shared home dashboard and a browser for device controls. An operator needs to edit dashboards without giving other household members access to rules, raw events, or operational traces.

## Decision

Store the versioned dashboard catalog on the authority in SQLite, alongside device State and outside the replicated State tree. The operator selects one home dashboard and publishes ordered layouts with direct device references and live selectors. Resolve membership against effective catalog metadata on the authority.

Use the existing browser session flow with distinct operator and household credentials. An operator-managed sharing grant projects a reserved tag into the device catalog. Household reads and typed controls check that grant. The Solid app shares the device snapshot and card renderers across dashboards, the device browser, and editor previews.

## Alternatives considered

- Browser-local layouts would be easy to prototype and would need separate synchronization across devices.
- Reusing the operator credential would give each household member access to automation editing and raw data.
- Granting access through integration-provided tags would make the integration responsible for deciding what the household can access.

## Consequences

One authority owns the layout and permission changes. SQLite backups include dashboard definitions and device sharing. Household users see one shared home and can control devices explicitly shared by an operator. [Household dashboards](../household-dashboards.md)
