# Extension points

How something is plugged into nit without being part of it.

This exists because nit has a commercial edition built *around* this
repository, and the line between the two has to be legible to a contributor
who has never heard of it. The short version:

> **Open: the decision. Closed: the administration of what feeds it.**

`pkg/policy`, `pkg/enforce` and `pkg/patch` decide who may read and write which
files. They stay open, because that is precisely what an adopter of an
authorization layer needs to audit. What plugs in is everything that *fills*
their inputs — a directory, an approval workflow, an export, a different
database.

## The rule that has no exception

**No commercial code in this repository.** Not in an `ee/` directory, not
behind a build tag, not in a file that says "licensed differently".

The reason is not purity. It is that a contributor must be able to know the
licence of what they are writing from the fact that they are writing it here,
without checking which folder they are in. Projects that blur this spend the
next several years untangling it.

The commercial edition is a separate, private Go module that imports this one
and registers its own implementations of the interfaces below. It is the same
server with more things registered — not a fork, and not a patch set.

## Why the seams are here at all

Each one is useful without the commercial edition:

| Seam | A community use |
| --- | --- |
| `Authenticator` | mTLS, or a token in a hardware token |
| `policy.Source` | a bundle from S3, or generated from another system |
| `audit.Sink` | mirror the trail to a file, or to a message queue |
| `store.Store` | a backend this project does not ship |
| Grant overlay | a script that grants an hour of access and revokes it |

A seam that only serves the paid edition is a crippled product wearing an
interface. If one of these stops being useful on its own, it is in the wrong
place.

## The seams

### `Authenticator` — who is this

The community implementation resolves a bearer token against its SHA-256 in
`sessions`. An implementation can instead resolve an OIDC or SAML assertion, a
client certificate, or anything else, as long as it returns a principal the
policy bundle declares.

What it must not do is decide *what* that principal may do. That is the
engine's job and it stays in one place.

### `policy.Source` — where the rules come from

Already an interface: something that returns the compiled bundle in force. The
directory-watching loader is one implementation.

An implementation may compose: read the bundle from disk *and* merge group
membership from a directory, so that `subject: {type: group, id: platform}`
resolves against the company's real groups rather than a list checked into
git. The rules stay in files; only the membership comes from elsewhere.

### `audit.Sink` — where the record goes

Today every decision is appended to `audit_log`. A sink lets that fan out —
to a SIEM, to object storage, to a file.

Two properties a sink must preserve: a failure to write **must not** fail the
operation being recorded (audit is best-effort at request time, deliberately),
and it must never be the *only* copy. The database write stays.

### `store.Store` — where the state lives

Already an interface, and — more usefully — `storetest` is a conformance suite
that runs the same tests against any implementation. A backend is therefore
provable rather than hopeful; three real bugs in the PostgreSQL store were
found by running that suite against a live database rather than a mock.

Anyone writing a backend runs the suite. If it passes, the queue's exclusion,
fencing and idempotency guarantees hold.

### Grant overlay — temporary access

The engine gains a notion of a **grant**: a subject, a path set, an action set,
and an expiry. Evaluation considers grants alongside rules, and a grant that
has expired is simply not there.

The mechanism is open and scriptable. What is not in this repository is the
workflow around it — who may request one, who approves, what evidence is
attached. That is administration.

**A grant is not a rule.** Rules live in files, in git, reviewed like code, and
that does not change. A grant is a time-bound record that a rule already
authorises somebody to create.

## What is deliberately not a seam

**Enforcement.** `pkg/enforce` decides what a push may land and what a pull may
carry. There is one decision point and it is here. An implementation that could
replace it could disagree with it, and then the audit trail records a decision
that is not the one that was applied.

**The wire protocol.** Clients are a contract. It changes with a version number
and a plain sentence about what breaks, not with a plugin.

**Fail-closed pushes, per-branch serialization, one squashed commit per push.**
These are the product. They are recorded with their reasoning in
[`DECISIONS.md`](DECISIONS.md) — D1, D11, D4.

## Adding a seam

Open an issue first. A seam is a public interface, which is to say a promise,
and the bar is the table above: it has to be useful to somebody who is not
paying.

Anything that reaches the decision — a hook that could change an outcome, a
callback inside evaluation — is not a seam and will be declined. `Evaluate`
stays total, order-independent and attributable (D9), and it cannot be any of
those if arbitrary code runs inside it.
