# nit build targets.
#
# GOROOT is unset explicitly: gvm pins GOROOT to the Go version its pkgset was
# created with, which makes the downloaded toolchain named in go.mod disagree
# with its own tools ("compile: version go1.24.2 does not match go tool version
# go1.25.11"). Unsetting it lets the resolved toolchain find its own root.
GO := env -u GOROOT go

BIN := bin
CMDS := nit nitd nit-worker nitctl

.PHONY: all build test test-postgres race cover lint fmt vet tidy clean policy-check db-test-setup db-test-drop

# DSN of a throwaway database the store conformance suite may truncate.
TEST_DSN ?= postgres://postgres:postgres@localhost:5432/nit_test

all: build

build:
	@mkdir -p $(BIN)
	@for cmd in $(CMDS); do \
		echo "building $$cmd"; \
		$(GO) build -o $(BIN)/$$cmd ./cmd/$$cmd || exit 1; \
	done

test:
	$(GO) test ./...

# The store conformance suite against real PostgreSQL. The in-memory store
# passing is necessary but not sufficient: the concurrency bugs that matter
# (two workers claiming the same branch) only exist in the SQL.
test-postgres: db-test-setup
	NIT_TEST_POSTGRES='$(TEST_DSN)' $(GO) test -race -count=1 ./internal/store/postgres/

db-test-setup:
	@psql '$(TEST_DSN)' -c 'SELECT 1' >/dev/null 2>&1 || \
		( echo "create the test database first, e.g."; \
		  echo "  createdb nit_test && ./bin/nitctl migrate -dsn '$(TEST_DSN)'"; exit 1 )
	@./bin/nitctl migrate -dsn '$(TEST_DSN)' >/dev/null 2>&1 || true

db-test-drop:
	dropdb --if-exists nit_test

race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

# Validate the example bundle. Run the same target in CI against the real
# policy repository: a bundle that does not compile must never reach production.
policy-check: build
	$(BIN)/nitctl policy validate ./configs/policy/example

clean:
	rm -rf $(BIN) coverage.out
