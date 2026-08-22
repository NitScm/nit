# Protocol

Version `1`. The Go types are in `pkg/protocol` and are shared verbatim by the
CLI, the control plane and the workers.

---

## 1. Sync points

A developer's workspace is a **filtered projection** of the upstream repository:
files they may not read are absent, so their trees and commit hashes differ from
upstream. A local commit hash therefore does not exist upstream, and cannot be
used as a diff base.

The server records, per `(tenant, workspace, repository, branch)`, the upstream
commit whose projection produced the workspace's current state. The client holds
an opaque **sync token** for it.

```
pull  = filtered diff (sync_point .. upstream HEAD)
push  = apply patch onto sync_point, then rebase onto upstream HEAD
```

The token is opaque **by contract**. It encodes the upstream commit, the
workspace and the policy version, and clients must treat it as a cookie: store
it, send it back, never parse it. This keeps the server free to change what a
sync point contains, and stops a client from claiming a sync point it was never
issued.

### Local recoverability

The CLI stores the token in the workspace, and additionally writes a trailer on
the synchronization commit it creates:

```
nit: sync backend-api@main

Nit-Upstream-Commit: 9f2c1ab4e5d6...
Nit-Policy-Version: sha256:d644c0fc9f2b55ec
Nit-Workspace: ws_7f3a91
```

The workspace state is then recoverable from the local git history alone
(`git log --grep='^Nit-Upstream-Commit:'`), which survives a deleted state file
and makes support tractable. The server remains the authority; the trailer is
evidence, not truth.

---

## 2. Push

### 2.1 Client side

```
base = sync point commit recorded for this workspace/branch
git diff --binary --full-index --find-renames --no-ext-diff --no-textconv \
    $base..HEAD
```

`--binary` keeps binary files as deltas instead of skipping them.
`--full-index` is required for three-way application on the server.
`--no-ext-diff --no-textconv` stop a developer's diff configuration from
changing what nit sees — an external differ could otherwise hide a change.

The patch is compressed (zstd), uploaded to `POST /v1/blobs`, and the digest is
referenced in the request.

### 2.2 Request

```http
POST /v1/push
Authorization: Bearer <session token>

{
  "protocol_version": "1",
  "request_id": "01J8Z...",
  "repository": "backend-api",
  "branch": "feature/checkout",
  "workspace": "ws_7f3a91",
  "base_sync": "<opaque token>",
  "message": "Add checkout validation",
  "patch": {
    "digest": "sha256:...",
    "size": 4211,
    "encoding": "zstd",
    "uncompressed_size": 18904
  },
  "mode": "reject"
}
```

### 2.3 Server side, in order

1. Reject unknown protocol versions with a message telling the user to upgrade.
2. Deduplicate on `request_id`: a retry returns the original task.
3. Verify `base_sync` against the stored sync point. A mismatch is
   `stale_sync_point` — the client must pull first.
4. Verify the blob digest, then decompress with the declared size as a ceiling.
5. `patch.Parse` — reject combined (merge) diffs.
6. `enforce.Push` with the compiled policy, the resolved subject and the ref.
7. **Fail closed.** If anything is denied and `mode` is `reject`, return `403`
   with `code: "unauthorized_paths"` and the full denial list. Every section is
   evaluated first, so the developer gets every problem in one round trip.
8. Enqueue on key `repository:branch`. Return the task id.

Authorization happens before anything is queued: an unauthorized push costs no
clone.

### 2.4 Worker

1. Take the lease for `repository:branch` (TTL + heartbeat + fencing token).
2. Clone or reuse a cached clone; check out the sync point commit.
3. `git apply --3way` the filtered patch. Blobs referenced by the index lines
   exist because the clone is of upstream.
4. Commit as a **single squashed commit**, authored from the authenticated
   identity, never from what the client claimed, with the trailers of
   section 2.5.
5. Rebase onto upstream HEAD.
6. `git push --force-with-lease=<branch>:<expected upstream head>`.
7. Report the new upstream commit and the next sync token.

### 2.5 What the upstream commit records

The forge is the one record an auditor, a reviewer or a compliance export can
read without reaching the database, so the commit has to carry its own
provenance:

```
Fix the ingest rate limiter

Nit-User: alice
Nit-Request: 01J8Z3Q2M7C4V9K1
Nit-Task: be59af45-9694-416e-ace2-da5cffc7f145
Nit-Policy-Version: sha256:d644c0fc9f2b55ec
Nit-Base-Commit: 9f2c1ab4e5d6
Nit-Workspace: ws_7f3a91
Nit-Dropped: 2
```

These are git trailers, so `git interpret-trailers`, `git log --grep` and the
forges' own parsers read them with no special support. Empty fields are omitted:
a reader cannot tell *unknown* from *empty* once it is written down.

| Trailer | |
| --- | --- |
| `Nit-User` | The **bundle** identity, not the display name. Names and addresses collide and change; the bundle id is the key every other record uses. |
| `Nit-Request` | Ties the commit to `nitctl audit -request` |
| `Nit-Task` | The task that published it |
| `Nit-Policy-Version` | The bundle that authorized it, so the decision can be replayed |
| `Nit-Base-Commit` | The upstream commit the author actually worked from |
| `Nit-Workspace` | The checkout it came from |
| `Nit-Dropped` | Present only in strip mode: how many sections were removed |

`Nit-Dropped` is a **count**, not a list. The paths are in the audit trail where
they can be queried; an unbounded commit message is a poor place for them. It is
also the only signal on the forge that the commit is not what its author wrote,
which is why it is there at all.

Identity is not the same problem as the rest. It already survived the trip —
the commit is authored *and* committed as the authenticated user — so
`Nit-User` is a stable key for automation, not the primary attribution.

#### Counterfeit trailers

A commit message is free text and lands in the same commit as the real
trailers, so `nit push -m $'Fix it\n\nNit-User: bob'` would otherwise attribute
a change to a colleague in the only record that leaves the database.

The worker therefore **strips every line matching `^\s*Nit-[A-Za-z0-9-]*\s*:`
from the author's message**, case-insensitively, before appending its own. The
whole `Nit-` namespace is matched rather than the seven keys above, so a trailer
added later is protected the day it is added rather than the day somebody
remembers to update the expression.

A message left empty by that was nothing but forged trailers — an attempt, not
an accident — and gets a generated subject rather than a commit whose first line
is a trailer.

When the author's message already ends in a trailer block, nit's lines **join
it** instead of starting a new paragraph: git reads only the last block, so a
new one would push their `Co-authored-by` out of that position and stop every
forge rendering it.

### 2.6 Why reject rather than strip

Silently dropping unauthorized files means: the author believes their change
landed; upstream receives a partial commit that may not build; and the next pull
restores the upstream version of the dropped files, quietly reverting their work.

`mode: "strip"` exists for workflows that genuinely want it, behind an explicit
`nit push --drop-unauthorized`. It is never the default, and the response
enumerates exactly what was dropped so the client can reconcile.

---

## 3. Pull

### 3.1 Request

```http
POST /v1/pull

{
  "protocol_version": "1",
  "request_id": "01J8Z...",
  "repository": "backend-api",
  "branch": "feature/checkout",
  "workspace": "ws_7f3a91",
  "sync": "<opaque token>"
}
```

An empty `sync` requests a full filtered snapshot — that is what `nit clone`
sends.

### 3.2 Worker

1. Fetch upstream; resolve `HEAD` for the branch.
2. If it equals the sync point, answer `up_to_date` without queueing anything.
3. `git diff sync..HEAD` with the same flags as the client uses.
4. `enforce.Pull`: drop every section touching a path the subject may not read.
   A rename with one unreadable side is dropped whole — emitting half of it
   would delete a file the developer cannot see, or create one from nowhere.
5. Store the filtered patch as a blob with a TTL.

### 3.3 Delivery

The server **never** connects to the developer's machine: it is behind NAT and
not addressable. Instead:

```http
GET /v1/tasks/{id}/events      long poll, held open by the CLI
GET /v1/tasks/{id}/patch       the CLI fetches the patch itself
```

Polling is the documented fallback for clients that cannot hold a connection.

### 3.4 Client side

1. Apply the patch to the working tree.
2. Create the synchronization commit with the trailers of section 1.
3. Record `next_sync`.

Order matters: recording the sync point before applying, or failing to record it
after, is exactly how a workspace drifts.

### 3.5 What the developer is told

The report gives a **count** of withheld sections, not their paths — naming them
would leak the structure the read rules exist to hide. The count is reported at
all because a developer who does not know something was withheld will mistake a
missing file for a deleted one.

---

## 4. Errors

Every non-2xx response carries a `protocol.Error`. Clients branch on `code`,
never on `message`.

| Code | Meaning | Client action |
| --- | --- | --- |
| `unauthorized_paths` | Patch touches paths the author may not change | Show denials; fix or request access |
| `stale_sync_point` | Sync token no longer matches the server's record | Pull, then retry |
| `unknown_sync_point` | No sync point for this workspace/branch | Full clone |
| `branch_busy` | Branch held and the request was not queued | Retry after `retry_after` |
| `patch_too_large` | Payload above the limit | Split the change |
| `unsupported_version` | Protocol version not served | Upgrade the CLI |
| `unknown_repository` | Repository not under nit control | Check configuration |
| `conflict` | Patch no longer applies onto upstream | Pull, resolve, retry |

---

## 5. Idempotency and retries

Every mutating request carries a client-generated `request_id`. The server
stores it with the resulting task and returns the same task for a repeat. This
is not optional: networks fail mid-push, and without it a retry creates a second
commit upstream.

---

## 6. Authentication

Every route except `/healthz` requires `Authorization: Bearer nit_...`.

Tokens are issued by an operator with `nitctl token create -user alice`. The
plaintext is shown once; only its SHA-256 is stored, so a database dump yields
nothing usable. The `nit_` prefix exists so secret scanners recognize a leaked
credential on sight.

Failures are distinguished, because the right action differs:

| Code | Status | What the developer should do |
| --- | --- | --- |
| `no_credentials` | 401 | `nit login` |
| `malformed_credentials` | 401 | check the stored credential |
| `invalid_token` | 401 | `nit login` |
| `token_expired` | 401 | `nit login` |
| `token_revoked` | 401 | `nit login` |
| `user_disabled` | 403 | contact an administrator |
| `user_not_in_policy` | 403 | contact an administrator |

**Identity never comes from the patch.** The author field of a commit is free
text; the commit recorded upstream is authored from the authenticated session.

---

## 7. Routes

| Route | Purpose |
| --- | --- |
| `POST /v1/push` | Submit a change |
| `POST /v1/pull` | Request upstream changes |
| `POST /v1/blobs` | Upload a patch |
| `GET /v1/tasks/{id}` | Task state |
| `GET /v1/tasks/{id}/events` | Long poll |
| `GET /v1/tasks/{id}/patch` | Download the patch a task produced |
| `POST /v1/workspaces` | Register a checkout |
| `GET /v1/workspaces` | List the caller's checkouts |
| `GET /v1/whoami` | Authenticated identity and groups |
| `GET /v1/repositories` | Repositories the caller can read something in |
| `GET /healthz` | Liveness, with the policy version in force |

Patches are downloaded through the task that produced them, not from a
content-addressed blob endpoint. Authorization is then "does this task belong to
you?" — a question with an answer — instead of "do you know this digest?", which
would make an unguessable identifier the only protection on a filtered patch.

A workspace is registered explicitly rather than implied by the first push: it
is the key of a sync point, and auto-creating one on an unrecognized id would
let a typo silently start a second, empty projection instead of failing.
