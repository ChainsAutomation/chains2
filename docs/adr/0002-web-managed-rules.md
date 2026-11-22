# ADR 0002 — Web-managed rules and live publication

**Status** Accepted · 2026-11-21

## Context

Operators need to author rules, apply them without restarting Chains, and move them between installations. Deployment rules may arrive from read-only files or Kubernetes ConfigMaps. Updating a web rule should preserve unrelated rule runs and schedules.

## Decision

The authority owns web rules. It stores DSL source as individual `.chains` files in a managed directory on a writable volume. A manifest records names, IDs, and enabled status. Each publish writes a complete generation and then atomically replaces the `CURRENT` pointer. Deployment files remain independently owned and appear read-only in the console. Workers continue loading their configured files.

The engine validates and prepares changes before committing them. Once the generation is selected, it activates the changed rules and cancels the affected runs and waits. Actions already submitted may finish. A catalog revision detects concurrent publication. A portable ZIP contains source and metadata for all authority rules. Import previews validation and name conflicts before publication.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| Store rule source in SQLite | Provides transactions and a compact store. File-based storage keeps each rule in its native DSL format on disk. |
| Edit the configured deployment files | Gives operators one set of files and conflicts with read-only mounts and deployment ownership. |
| Restart the runtime on every edit | Simplifies activation and interrupts unrelated runs and schedules. |
| Assign web rules to workers | Adds placement and ownership coordination across processes. The authority covers the current workflow. |

## Consequences

The managed rule directory needs persistent storage and belongs to one authority. Include it in backups or use **Download All** to move the rules separately from device State. An interrupted publication recovers from the generation selected by `CURRENT`. Changes to the deployment rules take effect through the usual restart of their engine. [Rules console](../operator-web.md#rules)
