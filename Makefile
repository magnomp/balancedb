# BalanceDB — maintenance interface. The Makefile is the contract; see CLAUDE.md
# (from M1.5) for what each target requires.

GO      ?= go
GOFUMPT ?= gofumpt

# Package list, computed once.
PKGS := ./...

.PHONY: all lint test itest tools fmt

all: lint test

## fmt: rewrite all Go files with gofumpt.
fmt:
	$(GOFUMPT) -w .

## lint: gofumpt formatting check + go vet. No third-party linter yet (M13).
lint:
	@unformatted="$$($(GOFUMPT) -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofumpt: the following files are not formatted:"; \
		echo "$$unformatted"; \
		echo "run 'make fmt'"; \
		exit 1; \
	fi
	$(GO) vet $(PKGS)

## test: unit tests (no external dependencies).
test:
	$(GO) test $(PKGS)

## itest: integration tests against TEST_DATABASE_URL. Stub until M2 wires
## the throwaway-schema harness; real integration tests arrive with migrations.
itest:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "itest: TEST_DATABASE_URL not set — skipping (no integration tests yet, M2)"; \
	else \
		echo "itest: no integration tests defined yet (M2)"; \
	fi

## tools: install pinned dev tooling into GOBIN.
tools:
	$(GO) install mvdan.cc/gofumpt@latest
