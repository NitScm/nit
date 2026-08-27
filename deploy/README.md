# Deploying nit

Container images and Compose stacks: one for development and user testing, and
three for real use — one per forge.

| Path | What it is |
| --- | --- |
| `Dockerfile` | All four nit binaries in one image |
| `dev/bootstrap.sh` | Seeds the dev environment and prints what to try |
| `dev/` | A complete environment with Gitea, ready in one command |
| `production/` | A forge-agnostic base plus one overlay per forge |

---

## Development and user testing

One command brings up PostgreSQL, a Gitea acting as the forge, nit, the API
description behind Swagger UI, and the web console — with a seeded repository
and three accounts whose access deliberately differs.

```sh
cd deploy/dev
docker compose up -d
docker compose logs -f bootstrap      # prints the tokens and what to try
```

| | |
| --- | --- |
| API | http://localhost:8080 |
| Console | http://localhost:4200 |
| Swagger UI | http://localhost:8081 |
| Gitea | http://localhost:3000 — `nit-admin` / `nit-admin-password` |

### Signing in to the console

```sh
docker compose exec nitd cat /var/lib/nit/tokens
```

Open http://localhost:4200, leave **Server** empty — nginx proxies `/v1` to the
control plane, so the console and the API share an origin — and paste **alice's**
token.

Alice is in `platform`, which is what `NIT_ADMIN_GROUPS` names. Bob and carol
sign in successfully but every page reports "not found": the operations API is
invisible to a non-operator, which is deliberate.

Three accounts, so switching between them shows the product doing something:

| User | Group | Access |
| --- | --- | --- |
| `alice` | platform | Everything, including `secrets/`, `infra/production/` and CI |
| `bob` | backend | `src/server`, `src/shared`, `docs` — no secrets |
| `carol` | frontend | `src/ui`, `src/shared`, `docs` — no secrets |

The bootstrap prints their tokens. Sign in to the console as **alice**: she is in
`platform`, which is what `NIT_ADMIN_GROUPS` names, and the operations API is
invisible to the other two.

Then try it as **bob**, who cannot read `secrets/`:

```sh
nit login http://localhost:8080 -token <bob's token>
nit clone backend-api
cd backend-api
ls                        # no secrets/, no infra/, no .github/ — they are not here
```

Change something he owns, and something he does not:

```sh
echo '// reviewed' >> src/server/api.go
git commit -am 'annotate'
nit push -m 'annotate the handler'          # lands

mkdir -p secrets && echo 'TOKEN=stolen' > secrets/prod.env
git add -A && git commit -m 'take a secret'
nit push -m 'should not land'               # refused, and the forge does not move
```

Everything is in named volumes; `docker compose down -v` removes all of it. The
bootstrap is idempotent, so `docker compose up -d` again converges rather than
failing on what already exists.

**The secrets in `dev/compose.yaml` are fixed and public on purpose.** It is a
development environment, and pretending a compose file is a secret store teaches
the wrong habit. Never copy it to a real deployment.

---

## Real deployments

A forge-agnostic base plus one overlay per forge. What differs between them is
small — the credential, and whether the forge is part of the stack — which is
why it is an overlay rather than three files that will drift apart.

```sh
cd deploy/production
cp .env.example .env && chmod 600 .env      # it holds two credentials
$EDITOR .env

docker compose -f compose.base.yaml -f compose.gitea.yaml  up -d   # self-hosted Gitea
docker compose -f compose.base.yaml -f compose.github.yaml up -d   # GitHub Cloud or Enterprise
docker compose -f compose.base.yaml -f compose.gitlab.yaml up -d   # GitLab, or any other forge
```

Add `--profile console` to include the web console.

### Before anything else

**Generate the signing key once and share it across every replica.**

```sh
openssl rand -base64 32 > /etc/nit/sync.key && chmod 600 /etc/nit/sync.key
```

A key generated per replica would make a load-balanced deployment reject sync
tokens at random. Rotating it makes every workspace resynchronize once. Treat it
like a database password.

**Put the policy bundle where `NIT_POLICY_DIR` points.** It is mounted
read-only, and it is where authorization actually lives. A checkout of your
policy repository, updated by your deployment, is the intended shape — the
bundle is reviewed like code, and `nitctl policy validate` belongs in its CI.

### What the base gives you

- `migrate` runs `nitctl migrate` and exits, before `nitd` starts. Migrations
  are not applied on boot: a schema change is a deployment decision, and a
  server that migrates on start-up will happily run half-rolled-out DDL from
  several replicas at once.
- `nitd` binds to loopback by default. Put a TLS terminator in front of it.
- `worker` scales with `--scale worker=4`. Pushes still serialize per branch —
  that is the queue's job — so more workers buy throughput across branches and
  repositories, never within one.
- The blob volume is **shared** between `nitd` and every worker. `nitd` writes
  the authorized patch there and a worker reads it back; separate volumes
  produce `missing_patch` on every push, which is the most common way to get
  this wrong.
- Secrets are mounted as files and named with `NIT_SYNC_KEY_FILE` and friends,
  so they never appear in `docker inspect` or a crash dump.

### The point worth pausing on, whichever forge

**nit is the only writer of the repositories it manages.** Its guarantees end
the moment a developer can push to the same branch directly. On every forge that
means the same three things:

1. Give nit's machine account write access, and remove everyone else's.
2. Protect the branches nit manages, with that account as the only permitted
   pusher.
3. Keep the repositories private. nit hides files *inside* a repository; it
   cannot hide a public one.

Without those, a developer who wants a file they may not read simply clones it
from the forge, and nit has authorized nothing.

### Another forge entirely

nit's git operations are plain clone, fetch and push, so **any remote git can
talk to already works** — Bitbucket, Azure DevOps, Gerrit, a bare repository over
SSH. The `forge:` field in the policy bundle selects a driver, and an
unrecognized value falls back to the generic one, which injects the token into an
`https://` remote and leaves `ssh://` remotes to git's own configuration.

A specific driver only becomes worth writing for the shortcuts — reading a branch
tip without cloning, opening a merge request — and neither is needed for push and
pull to work. `compose.gitlab.yaml` is the template: copy it, change the comment,
set the token.

---

## Building the images

```sh
# From the repository root.
docker build -f deploy/Dockerfile -t nit:local .

# The console is a separate repository; its Dockerfile lives with it.
docker build -t nit-console:local ../nit-console
```

The nit image carries all four binaries. They share most of their code, the whole
set is a few tens of megabytes, and an operator debugging a worker will want
`nitctl` on the same host; four images to build, tag, scan and keep in step would
buy nothing.

It ships `git`, `openssh-client` and `curl` because the worker's entire job is
clone, apply, rebase and push, and it runs under `tini` — a container running
thousands of clones accumulates the processes git leaves behind until it cannot
fork.

---

## When something does not come up

| Symptom | Cause |
| --- | --- |
| `schema "gitea" does not exist` | Gitea persisted `SCHEMA = gitea` into `/data/gitea/conf/app.ini` on its first start, and removing the environment variable does not remove the line. `docker compose up gitea-db` creates that schema; or clear it — see below. |
| `database "gitea" does not exist` | The `gitea-db` service did not run, or ran against a different PostgreSQL. `docker compose up gitea-db` creates it; it is idempotent. |
| `mkdir: can't create directory: Permission denied` | A named volume mounted at a path the image does not contain is created `root`-owned, and nit runs as uid 10001. `docker compose up shared-init` repairs it. |
| `missing_patch` on every push | `nitd` and the worker have different blob directories. They must share the volume. |
| Tasks stay `queued` | No worker is running, or `-queues` excludes that kind. |
| Tasks retry forever without failing | `NIT_LEASE_DURATION` is shorter than a clone takes. |
| `no sync token signing key` | `NIT_SYNC_KEY_FILE` names a path that is not mounted, or the key is under 32 bytes. |
| `not found` from `nitctl stats`, or on every console page | The account is not in `NIT_ADMIN_GROUPS`. Sign in as `alice`. |
| The console shows `unreachable`, or the sign-in screen rejects an empty Server | The console image predates the nginx proxy: `docker compose up -d --build console`. |

Gitea's database is prepared by a one-shot `gitea-db` service rather than a
`/docker-entrypoint-initdb.d` script, deliberately: those run **only when the
PostgreSQL data directory is empty**, so anyone adding Gitea to an existing
deployment would be stuck between an error and losing their volume. The service
converges either way and is safe to re-run.

**Gitea persists its configuration.** On first start it writes
`/data/gitea/conf/app.ini` from the `GITEA__*` environment, and it never removes
a key just because the environment stopped setting it. That is why an early
version of this file, which set `GITEA__database__SCHEMA`, leaves a volume asking
for a schema forever. Two ways out, and the compose file takes the first so
nobody has to:

```sh
# 1. Give it the schema it asks for — what gitea-db now does.
docker compose up gitea-db

# 2. Or clear the stale line and let it use the default.
docker compose exec gitea sed -i '/^SCHEMA/d' /data/gitea/conf/app.ini
docker compose restart gitea
```

When you change a `GITEA__*` variable and nothing happens, this is why: look at
`app.ini` before looking anywhere else.

**Named volumes inherit ownership from the image, but only where the image has
the directory.** Docker copies the contents *and* the owner of a path that
exists in the image into a fresh volume; where it does not exist, Docker creates
it `root`-owned. nit runs as uid 10001, so the shared volume is mounted at
`/var/lib/nit` — which the image creates and chowns — rather than at some path
of the compose file's choosing. The `shared-init` service repairs a volume
created before that was true.

---

## Where to look next

- `docs/CONFIGURATION.md` — every setting, its file key and its variable
- `docs/VALIDATION.md` — a walkthrough that proves each property in turn
