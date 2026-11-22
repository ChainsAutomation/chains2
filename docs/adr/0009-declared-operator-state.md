# ADR 0009 — Show declared attributes in operator State

**Status** Accepted

## Context

Operators need to see a service's attributes and capabilities before it reports a reading. The authority already stores registrations and observations independently, while the operator State projection historically omitted leaves without values.

## Decision

Project the declared tree from the same authority snapshot as its observations. Return attribute leaves even when no reading exists, omitting their value and timestamps. An explicit JSON null is a valid observation with timestamps. Keep the stored values and rule lookups based on actual observations.

Expose complete service descriptions to the CLI through the existing operator catalog endpoint.

## Alternatives considered

Seeding every declared attribute with null would create placeholder readings and obscure the first real observation. Joining independently fetched catalogs and samples in every client can mix revisions and duplicate logic.

## Consequences

Operators can inspect structure before devices report. Registration does not emit leaf transitions, and the first observation keeps its initial-transition semantics. [Operator CLI](../operator-cli.md)
