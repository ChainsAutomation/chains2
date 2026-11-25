# ADR 0011 — Bind rule templates to standalone DSL

**Status** Accepted

## Context

Reusable automations need to collect local devices and attributes from the authority's catalog. Published rules need to stay readable and portable.

## Decision

Store templates as TOML containing DSL and required device or attribute inputs. The authority validates the selections against registered capabilities and generates a normal rule draft with stable device IDs. Publish the draft through the existing rules catalog. Operator-uploaded templates have their own files on the same persistent volume.

## Alternatives considered

Keeping a template link in each published rule would allow rebinding and version upgrades in a form. It would add template dependencies to rule publication, runtime, and export. A standalone draft can use the existing rule editor, publication process, and file format.

## Consequences

Editing or deleting a template leaves previously generated rules intact. To apply a template change, regenerate a draft or edit the published DSL. Export template files separately to share the reusable definitions. [Rule templates](../rule-templates.md)
