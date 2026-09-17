# rsql

A storage service whose only interface is a **named, declared operation**.

No query text ever reaches the engine: an operation is declared once — its
access path, its limits, its permissions — stored in the database itself and
versioned. Clients call it by name. Cost is therefore known before execution,
injection cannot exist, and every call leaves a trace.

This repository holds the store: the engine, the protocol server, the CLI, the
Docker image and the desktop app. The client for TypeScript apps is
[`@ecosy/rsql`](https://github.com/material-atomic/ecosy-rsql).

## Layout

| Path | What |
| --- | --- |
| `cmd/rsqld` | The service |
| `cmd/rsql` | The CLI |
| `cmd/rsql-desktop` | The desktop app, for development |
| `internal/signing` | The signing contract, shared with the client through `fixtures/signing.json` |
| `build/docker` | The image |

## The shared fixture

`fixtures/signing.json` carries connection triples with their expected digests.
Both this repository's tests and `@ecosy/rsql`'s read it, so a change to the
contract turns both suites red at once — instead of arriving as a user who
cannot connect, with nothing in a log to say which side is wrong.
