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

### `store.Store` — where the state lives

The queue, sync points, sessions and the audit trail. Two implementations ship:
in-memory and PostgreSQL.

What makes this one worth writing against is `internal/store/storetest`: a
conformance suite that runs the *same* tests against any implementation —
partition exclusion, fencing, lease expiry, idempotency, concurrent claims. A
backend is therefore provable rather than hopeful. Three real bugs in the
PostgreSQL store were found by running that suite against a live database
rather than a mock.

If you write a backend, run the suite. If it passes, the queue's guarantees
hold.

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
has to be useful to somebody with no unusual requirements — the three above
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
