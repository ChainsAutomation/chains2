# ADR 0008 — Dashboard editing in the household app

**Status** Accepted

## Context

The household app had its own sign-in, but dashboard creation was only available with the operator token. Signing in with the household token left the main creation flow inaccessible.

## Decision

Both household and operator sessions can create and manage the shared dashboard catalog through the household app. The editor opens from Home and the permanent Dashboards page. The catalog, revision checks, and live selector resolution stay on the authority. Rules, traces, and placement editing use the operator console and its own authentication.

## Alternatives considered

An operator-only editor could add a prompt visible to household users. It would still require a second credential to complete the core dashboard workflow.

## Consequences

Everyone with the household token can change the shared home dashboard and edit or delete saved dashboards. Existing dashboards retain their IDs, layouts, and navigation settings. [Household dashboard guide](../household-dashboards.md)
