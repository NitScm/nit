# Extension points

Some of nit's behaviour is behind an interface so it can be replaced without
forking. This says which, what each one is for, and what is deliberately not
one.

## The seams

### `policy.Source` — where the bundle comes from

Returns the compiled bundle in force. The directory-watching loader is one
implementation; `policy.Static` is another, for a bundle decided once at
start-up.

The interface lives in `pkg/policy` rather than beside the loader because a
directory is *one way* to obtain a bundle, not the definition of one. An
implementation can read from object storage, generate rules from another
system, or compose: read the rules from files and resolve group membership
against a company directory, so `subject: {type: group, id: platform}` means
what that company already means by it. The rules stay in files; only the
membership comes from elsewhere.

One thing an implementation must preserve, because something now depends on it:
`Version()` has to change whenever any rule changes. The pull cache keys a
shared projection on a fingerprint that includes the version, so a bundle that
kept its version across an edit would serve projections computed under rules
that no longer apply. The directory loader hashes the bundle's content, which is
the property to copy.

`Current` is called on the request path, so it must be cheap and must not
block. Implementations that fetch from elsewhere refresh in the background and
serve the last good bundle meanwhile — which is what the directory loader
does, and why a bundle that fails to compile never changes anything.

### `audit.Sink` — where the record goes

Every decision is appended to `audit_log`. A sink lets that fan out as well —
to a file, a message queue, an object store.

Two properties an implementation must preserve:

- **A failed write must not fail the operation being recorded.** Audit is
  best-effort at request time on purpose: refusing an authorized push because a
  log write failed would be its own kind of outage. An error is logged loudly;
  the operation continues.
- **A sink is never the only copy.** The database write stays. An export that
  silently became the sole record would put the audit trail behind somebody
  else's availability.

`audit.Multi` fans out to several sinks and attempts every one even after a
failure, joining the errors. Stopping at the first would make the order of
configuration decide which destinations receive a record.

Set one on `server.Deps.AuditSink` or `worker.Deps.AuditSink`. Nil means
persist only, which is the default. Records reach a sink with the bundle's
repository identity rather than a database row id — a sink writes somewhere
that has never heard of this database, and handing it a primary key invites it
to join on one.

### `store.Store` — where the state lives

The queue, sync points, sessions and the audit trail.

What makes this one worth writing against is `pkg/store/storetest`: a
conformance suite that runs the *same* tests against any implementation —
partition exclusion, fencing, lease expiry, idempotency, concurrent claims. A
backend is therefore provable rather than hopeful. Three real bugs in the
PostgreSQL store were found by running that suite against a live database
rather than a mock.

If you write a backend, run the suite. If it passes, the queue's guarantees
hold.

The interface and the suite are in `pkg/` precisely so a backend can live
outside this module and still be held to the same tests:

```go
import "github.com/NitScm/nit/pkg/store/storetest"

func TestConformance(t *testing.T) {
    storetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}
```

`pkg/store` describes IO rather than performing it — the implementations that
open connections are in `internal/store/`.

Three ship today: in-memory, PostgreSQL, and MySQL/MariaDB. The suite is where
each one earned its place, and `ConcurrentDrain` is the test that matters — it
keeps a queue busy through claim, complete and reclaim cycles and asserts every
task is delivered exactly once. That is what a backend reconstructing atomicity
without `UPDATE … RETURNING` has to get right, and it is what caught an InnoDB
deadlock the MySQL backend had on its first working version.

### `blob.Store` — where patch payloads live

Uploaded push patches and generated pull patches, addressed by the SHA-256 of
their bytes. One implementation ships: a directory, in
`internal/blob/filesystem`.

**`pkg/blob/blobtest` is what makes this seam real**, the same way `storetest`
does for `store.Store`:

```go
import "github.com/NitScm/nit/pkg/blob/blobtest"

func TestConformance(t *testing.T) {
    blobtest.Run(t, func(t *testing.T) blob.Store { return open(t) })
}
```

It asserts the obligations below rather than describing them — that a failed
Put leaves nothing reachable, that a mismatched digest is refused *and* not
stored, that a missing blob is an error and not empty content, that concurrent
writers of the same bytes never see each other's half-written blob. If it
passes, the contract holds.

Content addressing is what makes this replaceable cheaply. A blob's name is
derived from its bytes, so an implementation has nothing to coordinate: writing
the same bytes twice is the same write, and nothing has to be told what a digest
means.

Three obligations an implementation carries, none of which the signatures state:

- **`Put` is atomic.** A reader must never see a partially written blob under a
  digest that looks complete. The digest is what the rest of the system trusts
  in place of re-reading the bytes, so a torn write is not a corrupt file — it
  is a patch that passes verification and applies the wrong thing.
- **`Put` verifies what it stored** against the announced digest rather than
  trusting the caller.
- **`Get` returns `ErrNotFound`** for a digest never written, rather than empty
  content. A push whose patch has gone missing has to fail; applying nothing
  would report success for a change that never landed.

The reason to write one is deployment shape rather than performance. The
filesystem store requires that `nitd` and every worker see the same directory —
one host, or a shared volume. An implementation backed by object storage removes
that constraint, which is why this is a seam rather than an internal detail.

Set one on `server.Deps.Blobs` and `worker.Deps.Blobs`. Both must be the same
store: `nitd` writes the authorized patch and a worker reads it back.

## What is deliberately not a seam

**Enforcement.** `pkg/enforce` decides what a push may land and what a pull may
carry. There is one decision point and it is here. Something that could replace
it could disagree with it, and then the audit trail records a decision that is
not the one that was applied.

**Anything inside evaluation.** A hook or callback that runs during `Evaluate`
would end its guarantee of being total, order-independent and attributable
(D9). A rule set whose meaning depends on what code was registered cannot be
reviewed.

**The wire protocol.** Clients are a contract. It changes with a version number
and a plain sentence about what breaks, not with a plugin.

**Fail-closed pushes, per-branch serialization, one squashed commit per push.**
These are the product, and their reasoning is in [`DECISIONS.md`](DECISIONS.md)
— D1, D11, D4.

## Proposing a new seam

Open an issue first. A public interface is a promise, and the bar is that it
has to be useful to somebody with no unusual requirements — the four above
each are.

## On the commercial edition

There is one, and it is built *around* this repository rather than into it: a
separate module that imports this one. **No commercially licensed code lands
here** — not in an `ee/` directory, not behind a build tag, not in a file that
says "licensed differently".

The reason is not purity. It is that you should be able to know the licence of
what you are writing from the fact that you are writing it here, without
checking which folder you are in. Everything in this repository is Apache 2.0,
including every seam above, and an implementation you contribute stays that
way.

What that means in practice, using `blob.Store` as the example: the interface is
here and Apache 2.0, the filesystem implementation is here and Apache 2.0, and
the S3-compatible implementation the commercial edition ships is not. Nothing
stops you from writing your own against the same interface — the seam is not a
stub waiting for a licence key, and if it ever behaves like one, that is a bug
worth reporting.
