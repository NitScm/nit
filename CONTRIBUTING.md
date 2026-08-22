# Contributing to nit

The Go module: the authorization engine, the control plane, the workers and the
command-line tools.

Reporting a **security issue**? Do not open an issue — see
[SECURITY.md](SECURITY.md).

Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Everything is in English

Code, comments, commit messages, documentation, issues. This is a community
project and English is the language it works in. It is fine if it is not your
first language — a clear sentence beats a polished one.

## Getting set up

Requires **Go 1.25** and **git**.

```sh
make build      # the four binaries into bin/
make test
make race       # run this before opening a pull request
make vet
make fmt
```

Tests that need PostgreSQL are skipped unless you point them at one:

```sh
make db-test-setup      # creates the test database
make test-postgres
```

The store conformance suite runs the *same* tests against the in-memory and the
PostgreSQL backends. If you add a store method, it belongs in
`internal/store/storetest` — not in one implementation's own tests. Three real
bugs were found by running that suite against a live database rather than a
mock.

## The layering rule

The convention most likely to trip you up, and the one we care about most:

**`pkg/` is pure. `internal/` does the IO.**

`pkg/policy`, `pkg/enforce` and `pkg/patch` decide who may read and write what.
They take values and return values: no database, no filesystem, no network, no
clock. That is what makes them testable without infrastructure, and testable
without infrastructure is what makes them trustworthy.

`internal/` is the wiring — stores, queues, HTTP, git execution, configuration.

If a change to `pkg/` seems to need IO, it almost certainly belongs one layer
out. Open an issue before working around it.

```
pkg/patch      split a patch at diff --git boundaries, byte-exactly
pkg/policy     compile a bundle, evaluate a request
pkg/enforce    push and pull enforcement, guards
pkg/gitx       the git interface (the exec implementation lives here)
pkg/forge      hosting-provider drivers
pkg/protocol   the wire types

internal/…     store, queue, blob, auth, server, worker, client, flow, …
cmd/…          nit, nitd, nit-worker, nitctl
```

## Tests

**A change to the authorization path needs a test that fails without it.** The
engine, the patch splitter and the enforcement layer *are* the product; a
regression there is a leak, not an inconvenience.

- `pkg/` tests run with no database, no network and no git.
- `internal/` tests may use `t.TempDir()`, a real git binary and an in-memory
  store. The worker tests run against a real bare repository on disk precisely
  so the clone/apply/rebase/push cycle is exercised for real rather than against
  a mock that agrees with the implementation by construction.
- Comment *why* a test exists when it guards something subtle. Several existing
  tests describe the bug they were written for. That is deliberate — keep it up.
- Before trusting a new test, check it **fails** without your change. A test
  that passes either way is worse than none.

## Design decisions

[`docs/DECISIONS.md`](docs/DECISIONS.md) records every structural choice and its
reasoning, numbered D1 onward.

**If your change contradicts one, say so in the pull request and add an entry
superseding it.** A decision that turns out to be wrong should be replaced
there, not quietly worked around in code. If your change makes a choice a future
reader would find surprising, add an entry even when nothing is superseded.

Some things are deliberately settled, each with an entry explaining why:
fail-closed pushes (D1), a single squashed commit per push (D4), evaluation
being total and order-independent (D9), per-branch serialization (D11), identity
never coming from the patch (D19), commit trailers and message sanitization
(D30).

## Policy rules and the validation dataset

If you change how rules are compiled, matched or evaluated, extend
[`examples/github-ssh/check-policy.sh`](examples/github-ssh/check-policy.sh)
with cases covering it. That table is a real regression suite: a rule change
that quietly widens access fails it, offline, with no server and no database.

Denial cases matter most. An allow that stops working is reported by a developer
within the hour; a deny that stops working is reported by nobody.

## Style

- `gofmt` — `make fmt`. Unformatted code is not accepted.
- `make vet` clean.
- Comments explain **why**, not what. The code already says what it does.
- Errors say what to do about them. `nitctl config show` printing where each
  value came from, and a denial naming the rule that refused it, are the
  standard to match.
- Never log a credential. The authenticated remote URL is a credential, which is
  why a clone failure reports only the branch it was cloning.
- Match the surrounding code's density and idiom rather than importing your own.

## Commits and pull requests

- One logical change per pull request. A refactor and a fix are two.
- Present tense in the subject: *Reject a patch that renames into a protected
  path*.
- Explain the **why** in the body; the diff shows what.
- Say what you tested, and what you did not.
- Draft pull requests are welcome for early feedback.

## Documentation

The engineering documents in [`docs/`](docs) are the authority on internals:
`ARCHITECTURE.md`, `PROTOCOL.md`, `POLICY.md`, `CONFIGURATION.md`,
`VALIDATION.md`, `SCALING.md`, `DECISIONS.md`.

User-facing behaviour changes also go in `nit-docs/`, which has reference pages
for every flag, setting and error code. A documented flag that no longer exists
is a bug report waiting to happen.

If you change a route or a request type, update `api/openapi.yaml`. Tests assert
that every registered route is described and every described path has a route,
so a mismatch fails the build rather than the reader.

**Do not document behaviour you have not run.** If you cannot verify something,
write what you did verify and say what is unverified. The documentation makes
that distinction elsewhere and should keep making it.

## Good first contributions

- Error messages that do not say what to do next.
- Documentation that does not match the code.
- Cases for the policy validation dataset.
- `LISTEN`/`NOTIFY` for task events ([`docs/SCALING.md`](docs/SCALING.md) §7) —
  self-contained, and it removes a real per-client database load.

## Larger changes

Open an issue first. Not for gatekeeping: the design documents explain why
things are as they are, and it is faster to point you at the relevant decision
than to review a change a paragraph in `DECISIONS.md` already rules out.

## Licensing of contributions

Contributions are accepted under the [Apache License 2.0](LICENSE), per section
5 of that licence: unless you state otherwise, anything you intentionally submit
for inclusion is licensed under the same terms. There is no separate CLA.
