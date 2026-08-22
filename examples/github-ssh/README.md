# Example: workers against GitHub over SSH

A complete, runnable example: one repository on github.com reached over SSH,
three developers with deliberately different access, and a validation dataset
that proves the rules do what they claim.

The walkthrough — machine account, host keys, branch protection, seeding the
repository, running the workers, and the end-to-end checks — is the
**Worked example: GitHub over SSH** page of the documentation site
(`nit-docs/src/content/docs/guides/example-github-ssh.md`). These are the files
it uses; copy the directory and edit the two or three values that are yours.

| | |
| --- | --- |
| `policy/` | The bundle: three users, four groups, seven rules |
| `check-policy.sh` | 18 access cases with their expected outcome |
| `nit.yaml` | Worker configuration |
| `compose.yaml` | Two workers, overlaying `deploy/production/compose.base.yaml` |

## Quick start

```sh
nitctl policy validate ./policy
./check-policy.sh ./policy
```

```
  ok    maya   write    src/api/handlers.go
  ok    maya   write    src/ledger/posting.go
  …
all cases match
```

Both run offline against the files — no server, no database, no forge. That is
what makes them usable in the policy repository's CI, which is where they earn
their keep: a rule change that quietly widens access fails there instead of in
production.

## What is not here

`secrets/` and `known_hosts`, because both are yours:

```sh
mkdir -p secrets && chmod 700 secrets

ssh-keygen -t ed25519 -N '' -C 'nit@acme' -f secrets/id_ed25519
openssl rand -base64 32 > secrets/sync.key
echo 'postgres://nit:…@db:5432/nit?sslmode=require' > secrets/database-url
chmod 400 secrets/*

ssh-keyscan github.com > known_hosts        # then verify the fingerprints
```

Verifying those fingerprints against the list GitHub publishes is a step, not a
formality: `ssh-keyscan` trusts whatever answers, so accepting its output
unchecked is the machine-in-the-middle you were guarding against.

An SSH key with a passphrase cannot work here. The worker runs git with
`GIT_TERMINAL_PROMPT=0`, so a prompt becomes an error — which is the right
outcome, since the alternative is a worker hanging with a branch leased.
