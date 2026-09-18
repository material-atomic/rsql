# sapedb

A storage service whose only interface is a **named, declared operation**.

No query text ever reaches the engine. An operation is declared once — which
collection, which index, where along it, how many rows at most — and stored in
the database itself, versioned. Clients call it by name and pass arguments. An
argument cannot widen a scan, reach another collection, or become part of a key.

Three things follow, and they are the whole point:

- **Cost is known before anything runs.** A scan declares its limit, so there is
  no such thing as a query that turns out to be expensive in production.
- **Injection cannot exist**, because there is no text to inject into.
- **Every call leaves a trace** — who ran which operation, at which version, in
  the same log that replicas replay.

It is built for code nobody fully trusts: generated code, plugin code, code
written by an agent. The safety is in the storage layer rather than in a proxy
in front of it.

## What it is not

Said plainly, because a database that oversells itself costs somebody a quarter.

- **Not an analytics database.** One writer per database, and a commit means an
  fsync. Load spreads across tenants, not within one.
- **No interactive transactions**, ever. A client cannot hold one open. Several
  writes in one transaction is a declared *batch* operation instead.
- **No query language**, and no plan to add one. What exists for the questions
  nobody declared is an operator shell, which can only do what an operation
  could have declared, and which writes down everything it did.
- **No recovery path.** Not an omission: the commit order is data → sync → meta
  → sync, and a crash lands on a meta page describing a completed transaction.
  A recovery path is code that only runs when everything is already wrong, which
  makes it the least exercised code in the system.

## What is here

| Path | What |
| --- | --- |
| `cmd/sapedbd` | The server |
| `cmd/sapedb` | The CLI: apply, ls, dump, restore, log, url, shell |
| `internal/vfs` | The only thing that touches a disk, and a simulated disk that can be told to lie |
| `internal/pager` | Pages, checksums, two alternating meta pages, encryption at rest |
| `internal/btree` | Copy-on-write B+tree |
| `internal/keys` | Order-preserving key encoding |
| `internal/store` | Collections, indexes, operations, batches, the change log, partitions, rollups |
| `internal/protocol` | Frames |
| `internal/server` | The server, subscriptions, the operator channel |
| `internal/wire` | A small Go client, for the tools that ship with the server |
| `internal/signing` | The signing contract, shared with the client through `fixtures/signing.json` |
| `examples/ledger` | Order → payment → double-entry ledger, running |

The client for TypeScript applications is
[`@ecosy/sapedb`](https://github.com/material-atomic/ecosy-sapedb).

## The parts, briefly

**Operations.** Declared in a JSON file and applied with `sapedb apply`. Get by
key, scan a declared index between declared bounds, count, insert, put, update,
delete — and `batch`, which is several of those in one transaction, where a step
may require the document to already be in a particular state. That last part is
optimistic locking, written down in the schema where somebody deciding whether
to trust an operation can read it.

**Every operation is a transaction.** Atomic across the document, every index
entry and the log line; durable before the answer goes back; isolated because
there is one writer. Which is why there is no `begin`: there is nothing it would
add.

**The change log** is written in the same transaction as the change it
describes. One log serves replication, point-in-time recovery, change feeds and
audit, so they cannot disagree with each other. `subscribe(from)` streams it.

**Partitions** divide a collection into files, decided by the primary key —
by time (the key is a ULID, which already carries the millisecond it was made)
or by hash. Dropping one is an unlink. Indexes are local to their partition,
which is what keeps that true, and every consequence of it is checked where an
operation is declared rather than where it runs.

**Rollups** are totals kept by the transaction that changes them, so a count can
never disagree with the data it counts.

**The operator shell** (`sapedb shell`) is for the question nobody declared. It
can do exactly what an operation could declare, nothing more; it proves it holds
the server's own secret before it may; every access it makes goes into the
change log with a name against it; and `declare` prints the operation that would
do what you just did, so exploring ends in something to commit.

**Encryption at rest** is AES-GCM per page under a key derived from the secret.
The meta page is not encrypted, on purpose — something has to be readable
without the key or "not our file" and "wrong key" become one answer.

## Running it

    go build ./cmd/sapedbd ./cmd/sapedb

    export SAPEDB_SECRET=... SAPEDB_DIR=/var/lib/sapedb SAPEDB_ACCOUNT=acme SAPEDB_DB=main
    sapedb apply schema.json
    sapedbd

SAPEDB_ACCOUNT and SAPEDB_DB each become one path component under SAPEDB_DIR,
and together they derive the encryption key when SAPEDB_ENCRYPT is set — so
each must be 1 to 64 characters of letters, digits, dot, dash or underscore,
and neither `.` nor `..` on its own. A name outside that is refused, not
adjusted or normalized.

The server refuses to start without TLS unless `SAPEDB_INSECURE=1` says you meant
it. The Docker image ships the server binary and nothing else — no shell, no
package manager, no libc.

To watch the example end to end:

    SAPEDB_SERVER_BIN=./sapedbd SAPEDB_CLI_BIN=./sapedb node examples/ledger/run.mjs

## How this is tested

Unit tests are the floor, not the ceiling.

**Mutation testing** is the standard: production code is changed in ways
somebody could plausibly write by mistake, and a test must go red. A mutation
that survives means a property nobody is checking — it gets a test, or a written
reason beside the code explaining why it cannot be observed from outside. There
are three such reasons in this repository and each one names itself.

**A simulated disk** that can cut power between any two writes, tear a write,
reorder writes and lie about `fsync`, deterministically from a seed. Durability
is stated as two tests: with honest syncs a committed root always reads back,
and with a lying sync a transaction can be lost and all that survives is that
the file opens and names the damage.

**Real runs across layers.** Binaries, a container, two processes, the
TypeScript client calling the Go server. Three contract mismatches between the
two repositories passed every unit test on both sides, because each side agreed
with itself. Only running them together found any of them.

## The shared fixture

`fixtures/signing.json` carries connection triples with their expected digests.
Both this repository's tests and `@ecosy/sapedb`'s read it, so a change to the
contract turns both suites red at once — instead of arriving as a user who
cannot connect, with nothing in a log to say which side is wrong.

It proves the two implementations of one *function* agree. It does not prove the
two sides *call* that function with the same arguments, which is a lesson this
project paid for once.
