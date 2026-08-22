#!/bin/sh
# Bring a fresh nit development environment to a state you can log into.
#
# It creates the Gitea repository and seeds it, renders the policy bundle
# against that repository, applies the schema, and issues the tokens. It is
# idempotent: running it again converges rather than failing on what exists.
#
# The last thing it prints is what you need to start using the environment.

set -eu

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }

GITEA_API="${GITEA_URL}/api/v1"
AUTH="${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}"
REPO_OWNER="${GITEA_ORG_REPO%%/*}"
REPO_NAME="${GITEA_ORG_REPO##*/}"

# The shared-init service already created these and set their owner; this is
# belt and braces for anyone running the script by hand.
mkdir -p /var/lib/nit/blobs /var/lib/nit/work /var/lib/nit/policy

# ---------------------------------------------------------------------------
# 1. Wait for the administrator the gitea-admin service creates.
# ---------------------------------------------------------------------------

log "Waiting for Gitea"

i=0
until curl -fsS -u "$AUTH" "${GITEA_API}/user" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -gt 60 ]; then
        echo "Gitea did not accept ${GITEA_ADMIN_USER} after 60 attempts." >&2
        echo "Check: docker compose logs gitea gitea-admin" >&2
        exit 1
    fi
    sleep 2
done

note "ready at ${GITEA_URL}"

# ---------------------------------------------------------------------------
# 2. A token for nit to push with.
#
# nit is the only writer of the upstream, so this is a machine identity. It is
# recreated each run because Gitea shows a token's value only once.
# ---------------------------------------------------------------------------

log "Issuing a forge token"

curl -fsS -u "$AUTH" -X DELETE \
    "${GITEA_API}/users/${GITEA_ADMIN_USER}/tokens/nit-worker" >/dev/null 2>&1 || true

FORGE_TOKEN=$(
    curl -fsS -u "$AUTH" -X POST "${GITEA_API}/users/${GITEA_ADMIN_USER}/tokens" \
        -H 'Content-Type: application/json' \
        -d '{"name":"nit-worker","scopes":["write:repository","read:user"]}' |
        sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p'
)

if [ -z "$FORGE_TOKEN" ]; then
    echo "could not create a Gitea token" >&2
    exit 1
fi

printf '%s' "$FORGE_TOKEN" > /var/lib/nit/forge-token
chmod 600 /var/lib/nit/forge-token
note "written to /var/lib/nit/forge-token"

# ---------------------------------------------------------------------------
# 3. The repository, seeded with a tree that shows filtering off.
# ---------------------------------------------------------------------------

log "Creating ${GITEA_ORG_REPO}"

if curl -fsS -u "$AUTH" "${GITEA_API}/repos/${GITEA_ORG_REPO}" >/dev/null 2>&1; then
    note "already exists"
else
    curl -fsS -u "$AUTH" -X POST "${GITEA_API}/user/repos" \
        -H 'Content-Type: application/json' \
        -d "{\"name\":\"${REPO_NAME}\",\"private\":false,\"default_branch\":\"main\",\"auto_init\":false}" \
        >/dev/null
    note "created"
fi

REMOTE="${GITEA_URL}/${GITEA_ORG_REPO}.git"
AUTHENTICATED_REMOTE="http://${GITEA_ADMIN_USER}:${FORGE_TOKEN}@gitea:3000/${GITEA_ORG_REPO}.git"

if git ls-remote "$AUTHENTICATED_REMOTE" 2>/dev/null | grep -q refs/heads/main; then
    note "already seeded"
else
    log "Seeding the repository"

    SEED=$(mktemp -d)
    cd "$SEED"

    git init -q --initial-branch=main .
    git config user.email nit-admin@example.com
    git config user.name "nit admin"

    mkdir -p src/server src/ui docs secrets infra/production .github/workflows

    cat > src/server/api.go <<'EOF'
package server

// Owned by the backend team.
func Handler() string { return "hello" }
EOF

    cat > src/ui/app.ts <<'EOF'
// Owned by the frontend team.
export const app = () => 'hello';
EOF

    echo '# backend-api'                    > docs/readme.md
    echo 'TOKEN=super-secret-do-not-share'  > secrets/prod.env
    echo 'region = "eu-west-1"'             > infra/production/main.tf
    echo 'name: ci'                         > .github/workflows/ci.yml

    git add -A
    git commit -qm "initial commit"
    git push -q "$AUTHENTICATED_REMOTE" main

    cd /
    rm -rf "$SEED"
    note "seeded with src/, docs/, secrets/ and infra/production/"
fi

# ---------------------------------------------------------------------------
# 4. The policy bundle, pointed at that repository.
# ---------------------------------------------------------------------------

log "Writing the policy bundle"

cp -r /policy-template/. /var/lib/nit/policy/
sed -i "s|__REMOTE__|${REMOTE}|" /var/lib/nit/policy/repositories.yaml

nitctl policy validate /var/lib/nit/policy

# ---------------------------------------------------------------------------
# 5. Schema and tokens.
# ---------------------------------------------------------------------------

log "Applying the schema"
nitctl migrate

log "Issuing nit tokens"

issue() {
    nitctl token create -user "$1" -label dev | grep -o 'nit_[A-Za-z0-9_-]*' | head -1
}

ALICE=$(issue alice)
BOB=$(issue bob)
CAROL=$(issue carol)

cat > /var/lib/nit/tokens <<EOF
alice=${ALICE}
bob=${BOB}
carol=${CAROL}
EOF
chmod 600 /var/lib/nit/tokens

# ---------------------------------------------------------------------------

cat <<EOF

────────────────────────────────────────────────────────────────────────────
  nit development environment is ready
────────────────────────────────────────────────────────────────────────────

  API          http://localhost:${NIT_DEV_API_PORT:-8080}
  Console      http://localhost:${NIT_DEV_CONSOLE_PORT:-4200}
  Swagger UI   http://localhost:${NIT_DEV_SWAGGER_PORT:-8081}
  Gitea        ${GITEA_URL}   (${GITEA_ADMIN_USER} / ${GITEA_ADMIN_PASSWORD})

  Three accounts, with deliberately different access:

    alice   platform    everything, including secrets/ and CI
    bob     backend     src/server, src/shared, docs — no secrets
    carol   frontend    src/ui, src/shared, docs — no secrets

  alice   ${ALICE}
  bob     ${BOB}
  carol   ${CAROL}

  Sign in to the console as alice; she is in the platform group, which is what
  NIT_ADMIN_GROUPS names.

  Try it as bob, who cannot read secrets/:

    nit login http://localhost:${NIT_DEV_API_PORT:-8080} -token ${BOB}
    nit clone backend-api
    cd backend-api
    ls                      # no secrets/, no infra/, no .github/

    echo '// reviewed' >> src/server/api.go
    git commit -am 'annotate'
    nit push -m 'annotate the handler'

  Then try what he may not do:

    mkdir -p secrets && echo 'TOKEN=stolen' > secrets/prod.env
    git add -A && git commit -m 'take a secret'
    nit push -m 'should not land'

  Tokens are also in /var/lib/nit/tokens inside the containers:

    docker compose exec nitd cat /var/lib/nit/tokens

────────────────────────────────────────────────────────────────────────────
EOF
