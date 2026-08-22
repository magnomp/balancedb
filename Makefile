# BalanceDB — maintenance interface. The Makefile is the contract; see CLAUDE.md
# (from M1.5) for what each target requires.

GO      ?= go
GOFUMPT ?= gofumpt

# Package list, computed once.
PKGS := ./...

.PHONY: all lint test itest tools fmt check-docs

all: lint test

## fmt: rewrite all Go files with gofumpt.
fmt:
	$(GOFUMPT) -w .

## lint: gofumpt formatting check + go vet + docs sync. No third-party linter yet (M13).
lint: check-docs
	@unformatted="$$($(GOFUMPT) -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofumpt: the following files are not formatted:"; \
		echo "$$unformatted"; \
		echo "run 'make fmt'"; \
		exit 1; \
	fi
	$(GO) vet $(PKGS)

## check-docs: AGENTS.md must stay in sync with CLAUDE.md (M1.5). A symlink
## satisfies this by construction; a plain file must be byte-identical.
check-docs:
	@if [ -L AGENTS.md ]; then \
		if [ "$$(readlink AGENTS.md)" != "CLAUDE.md" ]; then \
			echo "check-docs: AGENTS.md symlink must point at CLAUDE.md"; exit 1; \
		fi; \
	elif [ -f AGENTS.md ]; then \
		if ! cmp -s AGENTS.md CLAUDE.md; then \
			echo "check-docs: AGENTS.md and CLAUDE.md differ — re-sync (symlink AGENTS.md -> CLAUDE.md)"; exit 1; \
		fi; \
	else \
		echo "check-docs: AGENTS.md is missing — symlink it to CLAUDE.md"; exit 1; \
	fi

## test: unit tests (no external dependencies).
test:
	$(GO) test $(PKGS)

## itest: integration tests against TEST_DATABASE_URL. Each test self-isolates
## in a throwaway schema (internal/dbtest). Files are behind the `itest` build
## tag, so `make test` never touches the database.
itest:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "itest: TEST_DATABASE_URL is required (e.g. postgres://user:pass@host:5432/db?sslmode=disable)"; \
		exit 1; \
	fi
	$(GO) test -tags itest $(PKGS)

## tools: install pinned dev tooling into GOBIN.
tools:
	$(GO) install mvdan.cc/gofumpt@latest
