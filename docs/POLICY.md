# Policy

A policy bundle is a directory of YAML files. It is the source of truth, it
lives in its own git repository, and it is reviewed through pull requests — so
rules get history, blame and rollback, which a database row does not have.

Validate one with:

```sh
nitctl policy validate ./configs/policy/example
nitctl policy explain  ./configs/policy/example -repo backend-api -user bob -path secrets/prod.key
```

---

## 1. Layout

```
users.yaml
groups.yaml
repositories.yaml
repositories/<repository-id>/rules.yaml
```

Decoding is **strict**: an unknown field is an error, not a shrug. A typo that
silently disables a rule is the worst failure mode a security policy can have.

Every load computes a version — a SHA-256 over the whole bundle, file names
included — which is stamped on every decision and every audit record, so any
past decision can be replayed against the exact rules that produced it.

---

## 2. Evaluation semantics

For a given `(repository, ref, subject, path, action)`:

1. An unknown repository is denied.
2. A disabled user is denied.
3. **Every** matching rule is considered; there is no early exit.
4. If any matching rule denies → **deny**.
5. Otherwise, if any matching rule allows → **allow**.
6. Otherwise → **deny** (closed by default).

Two consequences worth stating plainly:

- **Rule order does not matter.** Rules can be reordered, split across sections
  or regrouped without changing behaviour.
- **Deny always wins.** An allow rule can never override a deny. Exemptions are
  expressed with `except` (section 5), not with a competing allow.

Pattern specificity affects only *which* rule is reported in the explanation,
never the outcome.

---

## 3. Path patterns

| Form | Meaning |
| --- | --- |
| `secrets/` | **Subtree**: the directory entry itself and everything under it |
| `**/*.env` | Glob, for files scattered across the tree |
| `src/*.go` | `*` does not cross `/` |
| `src/**/*.go` | `**` crosses `/` |
| `{docs,site}/**` | Alternation |
| `**` | Everything |

A trailing slash is the explicit marker for a subtree. Nothing is inferred from
the presence of a dot in the last segment: a rule that changes meaning because a
directory was named `v1.0` is an incident waiting to happen.

Subtree patterns match the directory entry itself as well as its contents, so
`secrets/` also covers a symlink or submodule placed at exactly `secrets`.

Patterns must be repository-relative: no leading `/`, no `.` or `..` segments,
no backslashes.

---

## 4. Actions

| Action | Covers |
| --- | --- |
| `read` | Seeing the file at all — a denied read means the file is absent from the workspace |
| `write` | Modifying an existing file |
| `create` | Adding a new file, or being the destination of a rename or copy |
| `delete` | Removing a file, or being the source of a rename |
| `admin` | Structural changes — see section 7 |

`read` and `write` alone are not enough. Deleting a file is not writing it, and
a reviewer granting write access to a config directory rarely means "and you may
delete everything in it".

How patch operations map to requirements:

| Operation | Requires |
| --- | --- |
| add | `create` on the new path |
| modify | `read` + `write` on the path |
| delete | `read` + `delete` on the path |
| rename | `read` + `delete` on the source, `create` on the destination |
| copy | `read` on the source, `create` on the destination |

A rename holds on **both** sides. Otherwise renaming becomes a way to move a
file out of a protected subtree.

---

## 5. Rules

```yaml
- id: backend-owns-server-code        # optional but strongly recommended:
                                      # this is what appears in denials and audit
  subject:
    type: group                       # user | group | any
    id: backend
  except:                             # optional; carves subjects out of `subject`
    - type: group
      id: contractors
  paths:
    - src/server/
    - src/shared/
  refs:                               # optional; empty means every ref
    - refs/heads/feature/**
  actions: [read, write, create, delete]
  effect: allow                       # allow | deny
  description: Backend engineers own src/server and src/shared.
```

`description` is shown to the developer when the rule denies them. A denial
nobody can act on becomes a support ticket — write these.

### `except`, and why it exists

Since deny always wins, "nobody reads `secrets/`, except the platform team"
cannot be written as a universal deny plus a team allow: the deny swallows the
allow. The alternative — enumerating every non-exempt group in the deny rule —
fails open the day a new group is created. `except` is the safe form:

```yaml
- id: secrets-are-platform-only
  subject: { type: any }
  except:
    - { type: group, id: platform }
  paths: [secrets/, infra/production/]
  actions: [read, write, create, delete, admin]
  effect: deny
```

### `refs`, and branch protection

```yaml
- id: no-direct-push-to-main
  subject: { type: any }
  except: [{ type: group, id: platform }]
  paths: ["**"]
  refs: [refs/heads/main]
  actions: [write, create, delete]
  effect: deny
```

Note that `read` is left out: the branch is protected against writes, not hidden.

---

## 6. Validated invariants

The bundle is rejected at load time if any of these fail:

- An **allow** rule granting `write`, `create` or `delete` must also grant
  `read`. Writing a file you cannot see means overwriting content blind, and you
  cannot produce a diff against a file absent from your workspace. (A **deny**
  rule may of course name `write` alone — that is how "read-only" is expressed.)
- Group inclusion must be acyclic.
- Every referenced user, group and repository must exist.
- No duplicate ids.
- Every pattern must be valid and repository-relative.
- An `except` entry of type `any` is refused: it would disable the rule silently.

---

## 7. Guards and the `admin` action

Path rules cannot see changes that subvert the model rather than violating it
directly. `pkg/enforce` therefore requires `admin`, on top of the ordinary write
requirements, for:

| Guard | Trigger | Why |
| --- | --- | --- |
| `protected_path` | `.github/`, `.gitlab-ci.yml`, `.gitattributes`, `.gitmodules`, `.nit/`, `Jenkinsfile`, … | CI runs with a full checkout and can print anything: write access to CI is read access to everything |
| `symlink` | A change producing mode `120000` | A symlink is a read of its target performed by whoever resolves it |
| `submodule` | A change producing mode `160000` | A submodule pointer injects content from outside the repository |

The full default list is `enforce.DefaultProtectedPaths`. Deployments extend it;
shortening it should be a decision, not an oversight.

Because guards are expressed as a requirement for `admin`, they need no separate
configuration language — granting `admin` on `.github/` to the platform team is
an ordinary rule:

```yaml
- id: platform-owns-ci
  subject: { type: group, id: platform }
  paths: [.github/, .gitattributes, .gitmodules]
  actions: [read, write, create, delete, admin]
  effect: allow
```

---

## 8. Groups

```yaml
- id: backend
  description: Backend engineers
  members: [alice]
  includes: [interns]      # every member of `interns` is also a member of `backend`
```

Membership is transitively resolved once, at compile time — not per path of
every patch.

---

## 9. Identity

```yaml
- id: alice                       # nit's own stable identity, never changes
  email: alice@example.com
  aliases: [alice@old-company.com]
  forge_logins:
    github: alice-example
  disabled: false                 # denied everything, whatever the rules say
```

`email` and `forge_logins` are used to attribute existing history and to verify
commit authorship. They are **never** used to authenticate: the author field of
a commit is free text that anyone can forge. Identity always comes from the
authenticated session.

---

## 10. Reviewing a change

`nitctl policy validate` says a bundle is well-formed. It does not say what a
change *does*, and past the size where one person knows every rule, that is the
question review has to answer.

```
nitctl policy diff <before-dir> <after-dir> [-widening] [-json] [-exit-code]
```

It expands every rule through group membership and reports, per person, what
they gained and lost:

```
carol
  now allowed   read  payments  config/**   r-config   via backend
  DENY REMOVED  read  payments  secrets/**  r-secrets  via any

2 people can reach more than before.
```

Three things about that output are deliberate.

**Four directions, not two.** Deny wins (section 2), so a gained deny is *less*
access and a lost deny is more. A tool reporting "added" and "removed" would put
the most dangerous change — a deletion — in the column people skim.

**`via` is a column.** "carol gained read on `config/**`" invites the question
"how?", and the answer is usually where somebody notices a group has grown a
member nobody meant to add. That change touches no rule, and its YAML diff is
one line in another file.

**Reordering reports nothing.** The engine considers every matching rule, so
rules can be reordered, split or regrouped freely. A tool that cried on every
reorder would be ignored within a week.

### In CI

`-exit-code` exits 1 when anything changed, as `git diff --exit-code` does. On a
pull request that touches the bundle, the output belongs in a comment: a change
that widens somebody's access should not merge without a human having read a
list of who, and to what.

### What it cannot tell you

It compares rules as they apply to people, not outcomes at every path. Paths are
infinite; any enumeration would be a sample, and a sample that missed the one
path that mattered would be worse than no answer.

For one path and one person, `nitctl policy explain` answers exactly. The two
are for different moments: `explain` is for a question somebody already has,
`diff` is for the change nobody thought to ask about.
