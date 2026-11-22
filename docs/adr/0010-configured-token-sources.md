# ADR 0010 — Configured bearer token sources

**Status** Accepted

## Context

A native operator CLI needs a repeatable setup that can retrieve an existing token from a password manager. The engine also needs to support an optional secret source alongside its current environment and flag settings.

## Decision

Use TOML for the hand-edited XDG client configuration, consistent with service configuration. Support a literal token or an argument array for an executable that prints it. Bind client file credentials to the URL in the same configuration. Preserve the engine's JSON format and give each HTTP credential its own optional command.

## Alternatives considered

An environment-only setup pushes password-manager integration into launch scripts. Passing a shell expression in the config makes quoting and expansion depend on the shell. The argument array lets Chains invoke the configured executable directly.

## Consequences

A client can start from a clean shell with its remote URL and token source in one file. Engine helpers run at startup, so changing a helper's stored token takes effect after restart. The CLI's earlier JSON file needs to be converted to TOML. [Operator CLI](../operator-cli.md)
