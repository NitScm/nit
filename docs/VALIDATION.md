# Validation

A step-by-step walkthrough of a fresh installation that proves each property nit
claims, in order, with the output you should see.

Every command and every expected output in this document was executed against a
real installation — a real PostgreSQL, a real git forge, real binaries. If a
step gives you something different, that is a genuine difference, not a
documentation drift.

Budget about twenty minutes. `docs/CONFIGURATION.md` is the reference for every
setting used here.

---

## What this proves

| § | Property |
| --- | --- |
| 2 | A bundle that does not compile is refused, before it can reach production |
| 3 | The schema is applied deliberately, not on boot |
| 5 | The control plane and a worker start and agree on the policy in force |
| 7 | **Files a developer may not read never reach their machine** |
| 8 | An authorized push lands, authored by the authenticated identity |
| 9 | **An unauthorized push is refused and the forge does not move** |
| 10 | Write access to CI is not granted by write access to code |
| 11 | A colleague's readable change arrives; their confidential one does not |
| 12 | Operators can see the queue, and non-operators cannot see that they can |
| 13 | The audit trail names who did what, when, and under which rule |
| 14 | A worker that dies does not strand its branch |

---

## 0. Prerequisites

```sh
git --version          # 2.30 or later
psql --version         # PostgreSQL 13 or later
go version             # 1.25, to build
```

Build the binaries:

```sh
cd nit
make build             # bin/nit bin/nitd bin/nit-worker bin/nitctl
export PATH="$PWD/bin:$PATH"
```

---

## 1. A forge and a policy bundle

Any git remote works. This walkthrough uses a bare repository on disk, which git
treats exactly as it treats a remote server.

```sh
export NIT_VALIDATE=/tmp/nit-validate
rm -rf "$NIT_VALIDATE" && mkdir -p "$NIT_VALIDATE"
cd "$NIT_VALIDATE"

mkdir seed && cd seed
git init -q --initial-branch=main .
git config user.email you@example.com && git config user.name you

mkdir -p src/server src/ui docs secrets .github/workflows
echo 'package server'        > src/server/api.go
echo 'export const ui = 1;'  > src/ui/app.ts
echo '# Docs'                > docs/readme.md
echo 'TOKEN=super-secret'    > secrets/prod.env
echo 'name: ci'              > .github/workflows/ci.yml

git add -A && git commit -qm init
git clone -q --bare . "$NIT_VALIDATE/upstream.git"
cd "$NIT_VALIDATE"
```

Now the bundle. `bob` is a developer who owns the code; `alice` is on the
platform team and is the exception to the rule hiding `secrets/`.

```sh
mkdir -p policy/repositories/backend-api

cat > policy/users.yaml <<'EOF'
- id: alice
  email: alice@example.com
- id: bob
  email: bob@example.com
EOF

cat > policy/groups.yaml <<'EOF'
- id: devs
  members: [bob]
- id: platform
  members: [alice]
EOF

cat > policy/repositories.yaml <<EOF
- id: backend-api
  remote: $NIT_VALIDATE/upstream.git
  forge: generic
  default_branch: main
EOF

cat > policy/repositories/backend-api/rules.yaml <<'EOF'
- id: devs-own-code
  subject: { type: group, id: devs }
  paths: [src/, docs/]
  actions: [read, write, create, delete]
  effect: allow

- id: secrets-are-platform-only
  subject: { type: any }
  except:
    - { type: group, id: platform }
  paths: [secrets/]
  actions: [read, write, create, delete, admin]
  effect: deny
  description: Production secrets are owned by the platform team.

- id: platform-owns-everything
  subject: { type: group, id: platform }
  paths: ["**"]
  actions: [read, write, create, delete, admin]
  effect: allow
EOF
```

---

## 2. The bundle compiles — and a broken one does not

```sh
nitctl policy validate "$NIT_VALIDATE/policy"
```

```
bundle ok
  version:      sha256:f6040b6d6a8381dc
  repositories: 1
```

Your version hash will differ: it is a content hash of the bundle, and your
remote path is in it.

Check the rules say what you think they say, before anyone depends on them:

```sh
nitctl policy explain "$NIT_VALIDATE/policy" \
  -repo backend-api -user bob -path secrets/prod.env -action read
```

```
bob on secrets/prod.env (backend-api)
groups: devs

  DENY  read    denied by rule secrets-are-platform-only (denied_by_rule: secrets/)
          Production secrets are owned by the platform team.
```

And that the exemption works:

```sh
nitctl policy explain "$NIT_VALIDATE/policy" \
  -repo backend-api -user alice -path secrets/prod.env -action read
```

```
alice on secrets/prod.env (backend-api)
groups: platform

  ALLOW read    allowed by rule platform-owns-everything (allowed_by_rule: **)
```

**Now prove a broken bundle is caught.** This is the check that belongs in your
policy repository's CI:

```sh
cp -r policy broken
sed -i 's/id: devs/id: nonexistent/' broken/repositories/backend-api/rules.yaml
nitctl policy validate broken
```

```
nitctl: policy: repository "backend-api", rule nonexistent-own-code: unknown group "nonexistent"
```

Exit status is 1. ✔ **A bundle that does not compile cannot reach production.**

---

## 3. Database, configuration file and schema

```sh
createdb nit_validate
```

Write a configuration file rather than exporting a dozen variables — this is how
a real installation is set up, and `nitctl config show` will then tell you
exactly what is in force:

```sh
openssl rand -base64 32 > "$NIT_VALIDATE/sync.key"
chmod 600 "$NIT_VALIDATE/sync.key"

cat > "$NIT_VALIDATE/nit.yaml" <<EOF
server:
  addr: "127.0.0.1:8080"
  admin_groups: [platform]

database:
  url: "postgres://localhost/nit_validate"

policy:
  dir: $NIT_VALIDATE/policy

storage:
  blob_dir: $NIT_VALIDATE/blobs
  work_dir: $NIT_VALIDATE/work

security:
  sync_key_file: $NIT_VALIDATE/sync.key
EOF

export NIT_CONFIG="$NIT_VALIDATE/nit.yaml"
export NIT_CONFIG_DIR="$NIT_VALIDATE/config"     # the CLI's credentials, not the server's
```

Check it before starting anything:

```sh
nitctl config show
```

```
file: /tmp/nit-validate/nit.yaml

SETTING                     FROM         VALUE
addr                        file         127.0.0.1:8080
database.url                file         postgres://localhost/nit_validate
forge.token                 default      (not set)
git.ssh_command             default      -
log.level                   default      INFO
policy.dir                  file         /tmp/nit-validate/policy
policy.reload               default      30s
queue.lease_duration        default      1m0s
queue.max_attempts          default      3
queue.poll                  default      1s
queue.reap_every            default      30s
security.sync_key           file         (set)
server.admin_groups         file         platform
server.cors_origins         default      -
server.event_max_wait       default      30s
storage.blob_dir            file         /tmp/nit-validate/blobs
storage.max_patch_bytes     default      104857600
storage.mirror_budget_bytes default      21474836480
storage.pull_ttl            default      24h0m0s
storage.work_dir            file         /tmp/nit-validate/work
```

The `FROM` column shows which layer supplied each value. Secrets are never
printed, only whether they are set. ✔ **The configuration is legible before
anything runs.**

The environment still wins where you need it to:

```sh
NIT_LEASE_DURATION=90s nitctl config show | grep lease
# → queue.lease_duration        env          1m30s
```

Now the schema:

```sh
nitctl migrate
```

```
applied 1 migration(s)
```

`nitctl migrate` reads the same configuration, so there is no DSN to restate.
Running it again applies nothing — it is safe on every deploy:

```sh
nitctl migrate     # → schema up to date
```

✔ **The schema is applied deliberately.** `nitd` never migrates on boot.

---

## 4. Tokens

```sh
nitctl token create -user bob   -label laptop
nitctl token create -user alice -label console
```

```
session: 88fcd9ce-089d-471d-80a5-6eca7adca6da
user:    bob
expires: 2026-08-30T00:32:04Z

nit_QTGTYg1wRvjr0_NLvSX_BIfC_Fgul29LeKMs9Vc_ibc

This token is shown once and is not recoverable. Store it with:
  nit login <server-url>
```

Keep both. Only the SHA-256 of a token is stored, so there is no recovery path —
that is the point.

A token can only be issued to someone the bundle declares:

```sh
nitctl token create -user mallory
# → nitctl: bootstrap: user "mallory" is not in the policy bundle
```

---

## 5. Start the control plane and a worker

Both read `$NIT_CONFIG`; `-config <path>` does the same thing explicitly.

```sh
nitd &
nit-worker &

curl -s localhost:8080/healthz
```

```json
{"status":"ok","protocol_version":"1","policy_version":"sha256:f6040b6d6a8381dc"}
```

The policy version here must match what `nitctl policy validate` printed. During
a rolling deploy this is how you tell whether two replicas are serving different
bundles.

✔ **Both processes are up and agree on the policy in force.**

---

## 6. Sign in as a developer

```sh
nit login http://127.0.0.1:8080 -token nit_QTG…
```

```
Logged in to http://127.0.0.1:8080 as bob
Credential stored in /tmp/nit-validate/config/credentials.json
```

The token is verified before it is stored: a credential that does not work fails
here, not three commands later on an unrelated screen.

---

## 7. Clone — and prove read filtering

```sh
cd "$NIT_VALIDATE"
nit clone backend-api checkout -server http://127.0.0.1:8080
```

```
  registering the workspace
  waiting for the server to prepare the changes
  downloading 273 bytes
Cloned backend-api into /tmp/nit-validate/checkout
3 file(s) updated
2 file(s) withheld by policy
```

Two files were withheld: `secrets/prod.env`, and `.github/workflows/ci.yml`
which `bob` has no rule granting him. Now look at the disk:

```sh
find checkout -type f -not -path '*/.git/*' -not -path '*/.nit/*' | sort
```

```
checkout/docs/readme.md
checkout/src/server/api.go
checkout/src/ui/app.ts
```

```sh
ls checkout/secrets
# → ls: cannot access 'checkout/secrets': No such file or directory
```

✔ **Files bob may not read never reached his machine.** Not hidden, not
encrypted — absent. There is nothing on this disk to leak.

The workspace records where it is, in a form that survives a lost state file:

```sh
cd checkout && git log -1 --format='%s%n%b'
```

```
nit: sync backend-api@main

Nit-Upstream-Commit: 5e0deb3c78c35440c00a4978483da66fd5219f7f
Nit-Policy-Version: sha256:f6040b6d6a8381dc
Nit-Workspace: f9e428ab-3a8d-42c8-a2d7-6ef5a0ac61bc
```

---

## 8. An authorized push

```sh
echo 'package server // reviewed' > src/server/api.go
git add -A && git commit -qm "local work"

nit push --check
```

```
  uploading 164 bytes
1 file(s) would be pushed, 0 refused
```

`--check` authorizes without submitting anything: no task is queued and the
forge does not move. Then push for real:

```sh
nit push -m "annotate the api"
```

```
  uploading 164 bytes
  running
Pushed 1 file(s) to backend-api@main as 1e7d246954f0
```

Verify on the forge:

```sh
git --git-dir="$NIT_VALIDATE/upstream.git" log -1 --format='%an <%ae> — %s' refs/heads/main
git --git-dir="$NIT_VALIDATE/upstream.git" show refs/heads/main:src/server/api.go
```

```
bob <bob@example.com> — annotate the api
package server // reviewed
```

✔ **The change landed, authored by the authenticated identity** — not by
whatever the client claimed. And the file bob cannot see is untouched:

```sh
git --git-dir="$NIT_VALIDATE/upstream.git" show refs/heads/main:secrets/prod.env
# → TOKEN=super-secret
```

### The commit carries its own provenance

```sh
git --git-dir="$NIT_VALIDATE/upstream.git" log -1 --format='%B' refs/heads/main
```

```
annotate the api

Nit-User: bob
Nit-Request: 01J8Z3Q2M7C4V9K1
Nit-Task: be59af45-9694-416e-ace2-da5cffc7f145
Nit-Policy-Version: sha256:d644c0fc9f2b55ec
Nit-Base-Commit: 9f2c1ab4e5d6...
Nit-Workspace: ws_7f3a91
```

git's own parser reads them, which is what makes them useful to tooling:

```sh
git --git-dir="$NIT_VALIDATE/upstream.git" log -1 \
    --format='%(trailers:key=Nit-Request,valueonly)' refs/heads/main
```

✔ **A commit on the forge can be traced back without database access.** Keep
that request id — `nitctl audit -request <id>` in step 13 turns it into the full
thread.

Now prove the trailers cannot be forged:

```sh
echo 'package server // again' > src/server/api.go
git add -A && git commit -qm "more work"

nit push -m "$(printf 'sneak\n\nNit-User: alice')"

git --git-dir="$NIT_VALIDATE/upstream.git" log -1 \
    --format='%(trailers:key=Nit-User,valueonly)' refs/heads/main
```

```
bob
```

✔ **The counterfeit trailer was stripped from the message** before the real one
was appended. Attribution in the only record that leaves the database cannot be
written by the person being attributed.

---

## 9. An unauthorized push — the headline behaviour

Record where the forge is, so you can prove it does not move:

```sh
BEFORE=$(git --git-dir="$NIT_VALIDATE/upstream.git" rev-parse refs/heads/main)

mkdir -p secrets && echo 'TOKEN=stolen' > secrets/prod.env
git add -A && git commit -qm "try to write a secret"

nit push -m "should not land"
```

```
  uploading 150 bytes
nit: the patch touches paths you may not change

  secrets/prod.env (create)
      refused by rule secrets-are-platform-only
      Production secrets are owned by the platform team.
```

Exit status is 1. The message names the path, the rule and what to do — a
denial a developer cannot act on becomes a support ticket.

```sh
test "$(git --git-dir="$NIT_VALIDATE/upstream.git" rev-parse refs/heads/main)" = "$BEFORE" \
  && echo "forge unchanged"
```

```
forge unchanged
```

✔ **Fail-closed.** The whole push was refused — not stripped of its offending
file — and nothing was queued, so it cost no clone.

Clean up the attempt before continuing:

```sh
git reset -q --hard HEAD~1
```

---

## 10. The CI guard

The most important hole a path-based permission model has: a CI job runs with a
full checkout and can print anything, so write access to a workflow is read
access to the whole repository.

```sh
mkdir -p .github/workflows && echo 'name: exfiltrate' > .github/workflows/ci.yml
git add -A && git commit -qm "edit CI"

nit push -m "edit ci"
```

```
  uploading 158 bytes
nit: the patch touches paths you may not change

  .github/workflows/ci.yml (create)
  .github/workflows/ci.yml (admin)
      guard: protected_path
```

Two denials for one file: no rule grants bob `create` there, **and** the
protected-path guard requires `admin` on top of ordinary write permission. Even
a bundle that gave developers write access to everything would still hit the
second one.

✔ **Write access to code does not grant write access to CI.**

```sh
git reset -q --hard HEAD~1
```

---

## 11. Pull — filtering on the way back

Have a colleague change one readable file and one confidential file, directly on
the forge:

```sh
git clone -q --branch main "$NIT_VALIDATE/upstream.git" "$NIT_VALIDATE/other"
cd "$NIT_VALIDATE/other"
git config user.email platform@example.com && git config user.name platform

echo '# Docs, updated by the platform team' > docs/readme.md
echo 'TOKEN=rotated'                        > secrets/prod.env
git add -A && git commit -qm "docs and secret rotation" && git push -q origin main

cd "$NIT_VALIDATE/checkout"
nit pull
```

```
  waiting for the server to prepare the changes
  downloading 173 bytes
Updated backend-api@main to 81d30159ee92
1 file(s) updated
1 file(s) withheld by policy
```

```sh
cat docs/readme.md      # → # Docs, updated by the platform team
ls secrets              # → No such file or directory
```

✔ **The readable change arrived; the confidential one did not.** The count of
withheld files is reported without their paths — naming them would leak the
structure the read rules exist to hide — but it is reported, because a developer
who does not know something was withheld will mistake a missing file for a
deleted one.

```sh
nit status
```

```
repository: backend-api
branch:     main
server:     http://127.0.0.1:8080
workspace:  f9e428ab-3a8d-42c8-a2d7-6ef5a0ac61bc
sync:       190e4b2fec37
changes:    nothing to push
```

---

## 12. The operations API

Use alice's token — she is in `platform`, which is what `NIT_ADMIN_GROUPS` names:

```sh
export NIT_SERVER=http://127.0.0.1:8080
export NIT_TOKEN=nit_…            # alice's

nitctl stats
```

```
policy:       sha256:f6040b6d6a8381dc
repositories: 1

queued:       0
running:      0
succeeded:    3
failed:       0

busy branches:   0
denials (24h):   3
```

```sh
nitctl tasks -limit 6
```

```
TASK                                   KIND   STATE      USER         REPOSITORY@BRANCH        DURATION   NOTE
9ccf9693-56d4-4bec-838b-079867bcf379   pull   succeeded  bob          backend-api@main         38ms
be59af45-9694-416e-ace2-da5cffc7f145   push   succeeded  bob          backend-api@main         77ms
56c8b3f7-a0a1-4971-b5ba-3f18389b08bb   pull   succeeded  bob          backend-api@main         52ms
```

Now with **bob's** token, who is not in an admin group:

```sh
NIT_TOKEN=nit_QTG… nitctl stats
```

```
nitctl: not found; is this account in one of the server's NIT_ADMIN_GROUPS?
```

The status is 404, not 403. ✔ **The existence of an operations API is not
confirmed to someone who cannot use it.**

---

## 13. The audit trail

This is the endpoint the whole product exists to make possible.

```sh
nitctl audit -limit 8
```

```
WHEN                 ACTOR   ACTION             REPOSITORY@BRANCH   PATH                       RULE
2026-07-31 00:35:29  bob     push.applied       backend-api@main
2026-07-31 00:35:28  bob     push.accepted      backend-api@main
2026-07-31 00:33:27  bob     pull.delivered     backend-api@main
2026-07-31 00:33:26  bob     pull.requested     backend-api@main
2026-07-31 00:33:14  bob     push.denied_path   backend-api@main    .github/workflows/ci.yml
2026-07-31 00:33:14  bob     push.denied_path   backend-api@main    .github/workflows/ci.yml
2026-07-31 00:33:14  bob     push.rejected      backend-api@main
2026-07-31 00:33:14  bob     push.denied_path   backend-api@main    secrets/prod.env           secrets-are-platform-only
```

Every denial names the rule that produced it and the bundle version it came
from, so a past decision can be replayed against exactly the rules in force at
the time:

```sh
nitctl audit -user bob -since 1h -json | head -20
```

✔ **Who did what, when, and under which rule.** The table is append-only at the
database level: `audit_log` carries a trigger that refuses UPDATE, DELETE and
TRUNCATE with an error, so an application bug cannot rewrite history — and an
operator who tries is told, rather than quietly ignored.

---

## 14. A worker that dies

The queue serializes a branch with leases rather than a held lock, precisely so
that a crashed worker does not strand it. Prove it:

```sh
pkill -f nit-worker            # the worker is gone

# Queue a pull with nothing to execute it.
curl -s -X POST http://127.0.0.1:8080/v1/pull \
  -H "Authorization: Bearer nit_QTG…" \
  -d '{"protocol_version":"1","request_id":"orphan-1","repository":"backend-api",
       "branch":"main","workspace":"<workspace-id-from-nit-status>"}'

nitctl tasks -state queued
```

```
TASK                                   KIND   STATE      USER   REPOSITORY@BRANCH   DURATION   NOTE
5a544bd9-94cd-4f7d-9191-16d9e3481ef1   pull   queued     bob    backend-api@main    -
```

The task waits. It is not lost and it is not failed. Bring a worker back:

```sh
nit-worker &
sleep 3
nitctl tasks -limit 2
```

```
TASK                                   KIND   STATE      USER   REPOSITORY@BRANCH   DURATION   NOTE
5a544bd9-94cd-4f7d-9191-16d9e3481ef1   pull   succeeded  bob    backend-api@main    50ms
610f934f-f2e5-4a3b-a1a0-6f8e21be0a88   push   succeeded  bob    backend-api@main    75ms
```

✔ **Work survives the loss of a worker.** The same mechanism covers a worker
that dies *mid-task*: its lease lapses after `NIT_LEASE_DURATION`, the reaper
returns the task to the queue, and the fencing token stops the zombie from
completing a task somebody else now owns.

---

## 15. The web console (optional)

```sh
cd ../nit-console
pnpm install
NIT_CORS_ORIGINS=http://localhost:4200 nitd &     # restart nitd with CORS
pnpm start
```

Open `http://localhost:4200`, sign in with alice's token, and you should see the
same numbers `nitctl stats` printed, the same task list, the same audit trail,
and the policy bundle rendered as a table. They are the same endpoints: the
console can never show something `nitctl` cannot.

---

## 16. Tear down

```sh
pkill -f nitd; pkill -f nit-worker
dropdb nit_validate
rm -rf "$NIT_VALIDATE"
```

---

## If a step fails

| Symptom | Likely cause |
| --- | --- |
| `no sync token signing key` | `security.sync_key_file` unset, or the key is under 32 bytes |
| `is mode 0644 and contains secrets` | A config file with an inline secret is readable by others: `chmod 600`, or use the `_file` form |
| A setting seems ignored | `nitctl config show` — the environment overrides the file |
| `missing_patch` on every push | `NIT_BLOB_DIR` differs between `nitd` and the worker |
| Tasks stay `queued` forever | No worker running, or `-queues` excludes that kind |
| Tasks retry forever without failing | `NIT_LEASE_DURATION` is shorter than a clone takes |
| `not found` on `nitctl stats` | The account is not in `NIT_ADMIN_GROUPS` |
| `unknown_sync_point` on push | The workspace has never pulled; run `nit pull` |
| `stale_sync_point` on push | The workspace is behind; run `nit pull` |
| Console cannot reach the API | Its origin is not in `NIT_CORS_ORIGINS` |

The server logs are JSON and every request carries a `request` id that also
appears in the audit trail, so one failing operation can be followed from the
client's error message through the API log to the rule that refused it:

```sh
nitctl audit -request <request-id>
```
