# nit build targets.
#
# GOROOT is unset explicitly: gvm pins GOROOT to the Go version its pkgset was
# created with, which makes the downloaded toolchain named in go.mod disagree
# with its own tools ("compile: version go1.24.2 does not match go tool version
# go1.25.11"). Unsetting it lets the resolved toolchain find its own root.
GO := env -u GOROOT go

BIN := bin
CMDS := nit nitd nit-worker nitctl

.PHONY: all build test test-postgres test-mysql test-mariadb test-stores race cover lint fmt vet tidy clean policy-check db-test-setup db-test-drop

# DSN of a throwaway database the store conformance suite may truncate.
TEST_DSN ?= postgres://postgres:postgres@localhost:5432/nit_test
MYSQL_TEST_DSN ?= root:nit@tcp(127.0.0.1:3308)/nit_test
MARIADB_TEST_DSN ?= root:nit@tcp(127.0.0.1:3307)/nit_test

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

# The same suite against MySQL and MariaDB. Both, not one: the two agree on
# everything nit uses, and the only way to keep believing that is to run it.
#
#   docker run -d --name nit-mariadb -e MARIADB_ROOT_PASSWORD=nit \
#       -e MARIADB_DATABASE=nit_test -p 3307:3306 mariadb:11
#   docker run -d --name nit-mysql -e MYSQL_ROOT_PASSWORD=nit \
#       -e MYSQL_DATABASE=nit_test -p 3308:3306 mysql:8.4
test-mysql: build
	./bin/nitctl migrate -dsn '$(MYSQL_TEST_DSN)'
	NIT_TEST_MYSQL='$(MYSQL_TEST_DSN)' $(GO) test -race -count=1 ./internal/store/mysql/

test-mariadb: build
	./bin/nitctl migrate -dsn '$(MARIADB_TEST_DSN)'
	NIT_TEST_MYSQL='$(MARIADB_TEST_DSN)' $(GO) test -race -count=1 ./internal/store/mysql/

# Every backend the store claims to support.
test-stores: test-postgres test-mysql test-mariadb

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
