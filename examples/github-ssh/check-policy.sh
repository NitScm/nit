#!/usr/bin/env bash
#
# Check the payments bundle against the expectations below.
#
#   ./check-policy.sh ./policy
#
# Every line is "user path action expected". Adding a case is adding a line,
# which is the point: a rule change that quietly widens access fails here
# instead of in production.

set -uo pipefail

BUNDLE="${1:-./policy}"
REPO=payments

CASES=$(
	cat <<'EOF'
maya   src/api/handlers.go            write   ALLOW
maya   src/api/metrics.go             create  ALLOW
maya   src/ledger/posting.go          read    ALLOW
maya   src/ledger/posting.go          write   DENY
maya   docs/runbook.md                write   ALLOW
maya   go.mod                         read    ALLOW
maya   go.mod                         write   DENY
maya   secrets/stripe.env             read    DENY
maya   deploy/terraform/main.tf       read    DENY
maya   .github/workflows/ci.yml       write   DENY
maya   .gitattributes                 admin   DENY
raj    src/ledger/reconcile.go        write   ALLOW
raj    src/api/handlers.go            read    ALLOW
raj    src/api/handlers.go            write   DENY
raj    secrets/hsm-pin.txt            read    DENY
nadia  secrets/stripe.env             read    ALLOW
nadia  deploy/terraform/main.tf       write   ALLOW
nadia  .github/workflows/ci.yml       admin   ALLOW
EOF
)

failures=0

while read -r user path action expected; do
	[ -z "$user" ] && continue

	got=$(nitctl policy explain "$BUNDLE" \
		-repo "$REPO" -user "$user" -path "$path" -action "$action" |
		awk '/^  (ALLOW|DENY)/ { print $1; exit }')

	if [ "$got" = "$expected" ]; then
		printf '  ok    %-6s %-8s %s\n' "$user" "$action" "$path"
	else
		printf '  FAIL  %-6s %-8s %s — expected %s, got %s\n' \
			"$user" "$action" "$path" "$expected" "${got:-nothing}"
		failures=$((failures + 1))
	fi
done <<<"$CASES"

echo
if [ "$failures" -ne 0 ]; then
	echo "$failures case(s) did not match the expected access"
	exit 1
fi

echo "all cases match"
