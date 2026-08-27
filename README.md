# nit

An authorization layer between developers and a git forge.

nit decides, file by file, what each developer may read and write in a
repository, and is the only writer of the upstream. Developers push and pull
through nit; files they may not read never reach their machine, and changes they
may not make never reach the forge.

No forge offers this: GitHub, GitLab and Gitea grant access per repository, not
per path, and none of them can hide a file *inside* a repository from someone
who can clone it.

> Status: works end to end. A developer clones a filtered projection, changes
> it, pushes it back and pulls colleagues' work — with confidential subtrees
> that never reach their machine. The operations API, `nitctl` and the web
> console in `../nit-console` are all there.
> See `docs/ARCHITECTURE.md` §9 for what is done, and `docs/SCALING.md` for what
> the next load will demand.

---

## How it works

```
developer          nit control plane            worker             forge
    |                     |                       |                  |
 nit push  ------------>  authorize the patch     |                  |
    |                     (fail-closed)           |                  |
    |                     queue per branch  ----> clone, apply  ---> push
    |                                                                 |
 nit pull  ------------>  queue a pull      ----> diff, filter        |
    |  <-------------------------------------  filtered patch        |
  apply locally
```

The developer's clone is a **filtered projection** of the upstream repository, so
its commit hashes do not exist upstream. Every exchange is expressed relative to
a server-recorded **sync point**. That single fact explains most of the design;
`docs/PROTOCOL.md` §1 covers it.

---

## Repository layout

```
pkg/          pure domain: no IO, no clock, fully testable
  patch/      git patch model and byte-exact filtering
  policy/     rules, patterns, decision engine, YAML bundles
  enforce/    patch x policy -> filtered patch + report
  protocol/   wire types shared by every binary
  gitx/       git operations behind an interface
  forge/      hosting provider abstraction

internal/     wiring and IO
  server/     the control-plane API
  auth/       tokens and sessions
  synctoken/  signed, opaque sync tokens
  policyloader/ the compiled bundle, hot-reloaded
  worker/     push and pull handlers
  client/     the API client used by cmd/nit
  workspace/  a developer's local checkout: state, credentials, git
  flow/       clone, pull and push
  store/      persistence; memory and PostgreSQL behind one conformance suite
  queue/      branch-partitioned queue, leases, fencing
  blob/       content-addressed blob store
  integration/ end-to-end tests over the whole loop

cmd/          nit, nitd, nit-worker, nitctl
configs/      example policy bundle
docs/         architecture, protocol, policy, decisions
migrations/   embedded SQL schema
testdata/     patch corpus
```

`pkg/` never imports `internal/` and does no IO. That is what makes the core
testable without a database, a network, or even git.

---

## Try it

The fastest path is the development stack — PostgreSQL, a Gitea forge, nit, the
web console, with a seeded repository and three accounts whose
access deliberately differs:

```sh
cd deploy/dev && docker compose up -d
docker compose logs -f bootstrap      # prints the tokens and what to try
```

Or from source:

```sh
make test          # no infrastructure required

# The store conformance suite against real PostgreSQL. The in-memory store
# passing is necessary but not sufficient: the concurrency bugs that matter
# only exist in the SQL.
createdb nit_test
./bin/nitctl migrate -dsn postgres://localhost/nit_test
make test-postgres

# Validate and explore the example policy bundle.
make policy-check

# Run the control plane against PostgreSQL.
createdb nit_dev
export NIT_DATABASE_URL=postgres://localhost/nit_dev
export NIT_POLICY_DIR=./configs/policy/example
export NIT_SYNC_KEY=$(openssl rand -base64 32)

./bin/nitctl migrate
./bin/nitctl token create -user alice -label laptop
./bin/nitd &
./bin/nit-worker &

curl -s localhost:8080/healthz
```

Then, as a developer:

```sh
nit login http://localhost:8080          # paste the token
nit clone backend-api
cd backend-api

# Files you may not read are simply not here.
ls

vim src/app.go
git commit -am "my change"

nit push --check                          # authorize without submitting
nit push -m "my change"
nit pull

go run ./cmd/nitctl policy show ./configs/policy/example

go run ./cmd/nitctl policy explain ./configs/policy/example \
    -repo backend-api -user bob -path secrets/prod.key
```

```
bob on secrets/prod.key (backend-api)
groups: frontend

  DENY  read    denied by rule secrets-are-platform-only (denied_by_rule: secrets/)
          Production secrets and infrastructure are owned by the platform team.
```

---

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Components, layering, flows, concurrency, data model, build order |
| [`docs/PROTOCOL.md`](docs/PROTOCOL.md) | Sync points, push and pull exchanges, errors, routes |
| [`docs/POLICY.md`](docs/POLICY.md) | Rule language, patterns, actions, guards, invariants |
| [`pkg/protocol/routes.go`](pkg/protocol/routes.go) | Every route this API serves, declared publicly |
| [`deploy/README.md`](deploy/README.md) | Container images and Compose stacks, per forge |
| [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) | Every binary, every setting, deployment shapes |
| [`docs/VALIDATION.md`](docs/VALIDATION.md) | Step-by-step walkthrough proving each property |
| [`docs/SCALING.md`](docs/SCALING.md) | What scales, what does not, and in what order to fix it |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | Every design decision and its reasoning |
| [`examples/github-ssh/`](examples/github-ssh) | A worked deployment, with a policy validation dataset |

**Read `docs/SCALING.md` before adopting nit for a monorepo where a large team
pushes to a single trunk.** That is the one shape it is worst at, and the limit
is architectural rather than a matter of tuning.

---

## Contributing

Code and comments are in English. The core packages are covered by tests that
need no infrastructure; please keep it that way — anything requiring a database
or a network belongs in `internal/`.

## Installing

Released binaries, Linux packages and checksums are on the
[releases page](https://github.com/NitScm/nit/releases). The shortest routes:

```sh
# Ubuntu / Debian — installs all four binaries and requires git
sudo apt install ./nit_0.1.0_linux_amd64.deb

# Anywhere, with Go 1.25
go install github.com/NitScm/nit/cmd/nit@latest

# Confirm what you have
nit version
```

Windows ships `nit` and `nitctl` — the tools a person runs. The server
components are Linux and macOS: they compile for Windows, but a worker clones,
applies, rebases and pushes through a real git, and that has not been exercised
there. Publishing a binary claims it works; that claim waits for a test.

Full instructions, including checksum verification and PowerShell:
<https://nitscm.github.io/docs/start/install/>

## There is a commercial edition

Not described here, and deliberately: what it is for belongs in the
documentation, and what it contains is somebody else's repository. Two facts
belong here, because they are facts about **this** one.

**Nothing in this repository is licensed differently.** No `ee/` directory, no
build tag, no file that says otherwise. You can know the licence of what you are
writing from the fact that you are writing it here.

**No seam here is a stub waiting for a key.** Every extension point in
[`docs/EXTENSIONS.md`](docs/EXTENSIONS.md) ships with a working implementation
and a conformance suite that proves one correct. If any of them ever behaves
like a placeholder, that is a bug worth reporting rather than a pricing
decision.

Why a company would want more than this — and, said plainly, who does not need
it — is at <https://nitscm.github.io/docs/enterprise/>.

## Building

Requires Go 1.25 and git.

```sh
make build      # all four binaries into bin/
make test
make race
make cover
```

## License

Copyright 2026 The nit Authors.

Licensed under the Apache License, Version 2.0. See [`LICENSE`](LICENSE), or
<http://www.apache.org/licenses/LICENSE-2.0>.

Apache 2.0 grants an explicit patent licence alongside the copyright one, which
matters for a project that decides who may read and write what: a permissive
licence without a patent grant leaves an adopter with a risk they cannot assess.

The authorization engine being open is not incidental either. `pkg/policy`,
`pkg/enforce` and `pkg/patch` decide who sees which files; a closed
implementation of that would ask you to take on trust the one thing you most
need to audit.

Unless you state otherwise, any contribution you intentionally submit for
inclusion in this work is licensed under the same terms, per section 5 of the
License.
