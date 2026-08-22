# Security policy

nit decides who may read and write each file in a repository. A defect in that
decision is not a bug with a security impact — it is the product failing at the
only thing it does. Reports are treated accordingly.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private vulnerability reporting on this repository
(Security → Report a vulnerability), or email
**`SECURITY_CONTACT_TO_BE_SET`**.

Please include:

- what you can read, write or bypass that you should not be able to;
- the policy bundle that reproduces it, minimised if you can;
- the version, commit or image tag;
- whether it needs an authenticated token, and with what rights.

A minimal bundle plus a patch that reproduces the behaviour is worth more than a
long description. `nitctl policy explain` output helps us see what you saw.

### What to expect

| | |
| --- | --- |
| Acknowledgement | Within 3 working days |
| Initial assessment | Within 10 working days |
| Fix or mitigation for a confirmed critical issue | As fast as we can, and we will tell you what "as fast as we can" means once we understand it |

We will keep you informed, credit you in the release notes unless you prefer
otherwise, and tell you plainly if we decide something is not a vulnerability
and why.

We do not run a paid bounty programme.

## What we consider a vulnerability

These are the failures the product exists to prevent. All are in scope:

**Read filtering leaks.** A user receives, in a clone or a pull, any part of a
file they are denied `read` on. Including partial content, including a path
appearing in a diff header, including through the size or timing of a response
in a way that reconstructs the hidden tree.

**Authorization bypass on write.** A push lands a change to a path the author
lacks `write`, `create` or `delete` on. Including through patch encoding tricks,
path normalisation, symlinks, renames, or anything `pkg/patch` splits
incorrectly.

**Guard bypass.** Changing a protected path — CI definitions, `.gitattributes`,
`.gitmodules` — or introducing a symlink or submodule without holding `admin`.

**Identity forgery.** A commit landing upstream attributed to someone other than
the authenticated user, or a forged `Nit-*` trailer surviving into an upstream
commit message.

**Sync token forgery.** Getting the server to apply a patch onto a base the
signature did not authorise, or replaying another workspace's token.

**Token handling.** Recovering a token from stored state, using a revoked or
expired token, or authenticating as another user.

**Cross-tenant access**, in any deployment that runs more than one tenant.

**Credential disclosure.** The forge token or an authenticated remote URL
appearing in a log, an error message, an API response or a task record.

**Operations API exposure.** Reading `/v1/admin/*` without membership of a
configured admin group.

## What is not a vulnerability

Not because we do not care, but because the answer is documentation rather than
a patch:

**Developers can still reach the repository directly.** nit's guarantees assume
its machine account is the *only* writer and that the repository is private. A
deployment where developers keep write access, where a CI job holds
`contents: write`, or where an admin can bypass branch protection is
misconfigured. See `nit-docs` → *Connecting a forge*.

**A public repository is readable.** nit hides files inside a repository. It
cannot hide a public one.

**The count of withheld files is disclosed on pull.** Deliberate: a developer
who does not know something was withheld will mistake a missing file for a
deleted one. The *paths* are never disclosed — if they are, that is a leak and
we want to hear about it.

**A denial names the rule that produced it.** Deliberate. A denial nobody can
act on becomes a support ticket.

**The console keeps its token in `localStorage`.** A bounded, documented choice
for an internal read-only tool, which is why the console loads nothing from any
other origin and enforces that with a Content-Security-Policy. A way to *make*
it load third-party code is very much a vulnerability.

**Availability.** nit serializes pushes per branch and clones per task; a slow
push is a known limit, with the arithmetic in `docs/SCALING.md`. A way
to make one user's push block others *beyond* that documented behaviour is in
scope.

**The development Compose stack has fixed public secrets.** It says so, in
several places. Using it in production is not a finding.

## Supported versions

The project has not made a stable release yet. Until it does, only the `main`
branch is supported and fixes land there. This section will be replaced with a
version table at 1.0.

## Deploying nit safely

The checklist in `nit-docs` → *Going to production* is the short version. The
three that matter most:

1. nit's machine account is the only account able to push the protected branches.
2. `security.sync_key` is at least 32 bytes, generated once, shared by every
   replica, and never rotated casually.
3. Secrets are delivered as files, and any configuration file holding an inline
   secret is mode 600 — nit refuses to read one that is not.

## Our own supply chain

Dependencies are few and deliberately boring. If you find a problem in one that
affects nit, report it here as well as upstream; we would rather hear it twice.
