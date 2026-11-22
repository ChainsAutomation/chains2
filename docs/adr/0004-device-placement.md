# ADR 0004 — Device placement and catalog ownership

**Status** Accepted · 2026-11-22

## Context

Dashboards and rules need to group devices across services by physical location or by a shared label. Integrations may know a default placement, while the operator knows how devices are arranged at the site. Device identity needs to survive moves and service restarts.

## Decision

A device's location is an ordered array of segments in the catalog. Tags are a separate set of labels. Dotted shorthand and whole-segment wildcards share one matcher across HTTP device queries and rule selectors.

Services supply defaults. The authority stores operator overrides by stable service and device ID, separately from registration data. Catalog revisions and snapshots replicate effective placement to workers. Committed observations and leaf-change notifications carry the placement at commit time. The dashboard groups devices from the current catalog and uses existing schemas and setters for controls.

## Alternatives considered

| Alternative | Tradeoff |
| --- | --- |
| Store a dotted location string | Concise for manual entry and requires escaping a dot in segment names. |
| Represent location as tags | Supports membership tests and leaves nesting and ancestor matches to conventions. |
| Put location in the State address | Makes grouping direct and changes stored paths, rule references, and targets when a device moves. |
| Store all placements in service configuration | Keeps configuration close to integrations and spreads site layout changes across several services. |

## Consequences

A placement edit can move a device in the dashboard while existing observations and action references keep their identity. Catalog projections remain separate from services' reported attribute values. Operators can clear overrides or restore service defaults. [Device locations and tags](../devices.md)
