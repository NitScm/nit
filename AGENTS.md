This document describes a set of rules and conventions maintainers use in this repo.

## PR structure

* PRs in this repo are kept to one commit only. Iterations on the PR
  should be folded into existing commit by amending.

## Commit message conventions

* Use an `Assisted-By:` footer (not `Co-Authored-By:`) to attribute LLM/agent involvement in commits
* The `Assisted-By:` footer must reference the actual model used, not a generic name
  * Example footer: `Assisted-By: anthropic/claude-opus-5`

* Never put a `Claude-Session:` footer, or any `claude.ai/code/session_…` URL, in
  a commit message. History has been rewritten twice to remove these; do not
  reintroduce them.

* Commit messages must use a conventional commits prefix

* **Never push. Committing is automatic; pushing is manual and belongs to the
  maintainer.** Leave commits local and say what is ready to go. A push is
  outward-facing and reverses badly — twice already this project has had to
  rewrite published history.

* Commit messages must include a `Change:` footer with an alphanumeric, dash-separated identifier
  * Example footer: `Change: flatten-invert-check`

## Committing work

* Always run the test command when committing changes to Go code:

  ```sh
  make test        # or: make race, before anything touching the queue or a worker
  ```

* `make vet` and `make fmt` must be clean. Unformatted code is not accepted.

* A change to the authorization path — `pkg/policy`, `pkg/enforce`, `pkg/patch` —
  needs a test that fails without it. Check that it does.

## Repo conventions

* `pkg/` performs no IO: no database, no filesystem, no network, no clock.
  `internal/` does. A change to `pkg/` that seems to need IO belongs one layer
  out.

  It may *describe* IO. `pkg/store` is the interface a backend implements and
  `pkg/store/storetest` is the suite that proves one correct; both are
  contracts, and neither touches anything on its own. The implementations live
  in `internal/store/`. The rule exists so the authorization path is testable
  without infrastructure, and an interface does not threaten that — a package
  that opened a connection would.

  **One package takes an exception: `pkg/nitd`.** It assembles and runs a
  server, so it opens a database and listens on a socket. It is allowed to
  because it is a *leaf* — nothing else in the module imports it, so its IO
  cannot reach the authorization path — and `pkg/nitd/boundary_test.go` fails
  if that stops being true. Do not import it from anywhere but `cmd/`.

* Design decisions are numbered in `docs/DECISIONS.md`. A change contradicting
  one adds an entry superseding it rather than working around it in code.

* Everything is in English: code, comments, commit messages, documentation.

* Never log a credential. An authenticated remote URL is a credential, which is
  why a clone failure reports only the branch it was cloning.

## Agentic work

* For creating temporary plans, files, and experiments, use the gitignored `.agents/work/` folder
  in the root of the repo
* Inside, create subfolders matching to current work topic
  * Example folder: `.agents/work/clone-cache`
  * Example file: `.agents/work/clone-cache/PROGRESS.md`
