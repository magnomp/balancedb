# BalanceDB — maintenance interface. The Makefile is the contract; see CLAUDE.md
# (from M1.5) for what each target requires.

GO      ?= go
GOFUMPT ?= gofumpt

# Package list, computed once.
PKGS := ./...

.PHONY: all lint test itest simtest tools fmt check-docs openapi openapi-gen loadgen smoke image

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

## simtest: deterministic simulation harness (spec §15). The reference-model
## property tests are pure; the DB-backed simulation (build tag `simtest`) drives the
## real processor over a throwaway schema and asserts the database against the
## reference model, so it requires TEST_DATABASE_URL. Grows milestone by milestone
## (M6 covers groups + batching; M10 is the full harness).
simtest:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "simtest: TEST_DATABASE_URL is required (e.g. postgres://user:pass@host:5432/db?sslmode=disable)"; \
		exit 1; \
	fi
	$(GO) test -tags simtest ./simtest/...

## openapi-gen: (re)generate the committed contract from the code (ADR-0003).
## The server is the authority; this writes its OpenAPI 3.1 document to
## api/openapi.yaml. No database needed.
openapi-gen:
	$(GO) run ./cmd/openapigen -o api/openapi.yaml

## openapi: regenerate api/openapi.yaml, fail if it drifts from the committed
## copy (CI diff gate), then run an oasdiff breaking-change check against the
## previously committed spec. oasdiff is optional: if it is not installed the
## breaking-change check is skipped with a clear message (it belongs in CI).
openapi: openapi-gen
	@if ! git diff --quiet -- api/openapi.yaml; then \
		echo "openapi: api/openapi.yaml is out of date — it was regenerated from the code."; \
		echo "        Commit the regenerated file (ADR-0003: the spec is a committed artifact)."; \
		git --no-pager diff -- api/openapi.yaml; \
		exit 1; \
	fi
	@echo "openapi: api/openapi.yaml is up to date."
	@if command -v oasdiff >/dev/null 2>&1; then \
		if git cat-file -e HEAD:api/openapi.yaml 2>/dev/null; then \
			git show HEAD:api/openapi.yaml > /tmp/balancedb-openapi-base.yaml; \
			echo "openapi: checking for breaking changes vs HEAD..."; \
			oasdiff breaking /tmp/balancedb-openapi-base.yaml api/openapi.yaml; \
		else \
			echo "openapi: no committed baseline yet — skipping breaking-change check."; \
		fi; \
	else \
		echo "openapi: oasdiff not installed — skipping breaking-change check (runs in CI)."; \
	fi

## loadgen: run the local load generator against a running api node (plan §M9),
## to watch loop utilization rho, queue depth, and batching on :9090/metrics.
## Pass flags via ARGS, e.g. make loadgen ARGS="-rate 200 -duration 1m".
loadgen:
	$(GO) run ./cmd/loadgen $(ARGS)

## image: build the production Docker image (multi-stage, distroless, plan §M11).
image:
	docker build -t balancedb:local .

## smoke: end-to-end smoke test (plan §M11 "Done when"). Builds the image, brings
## up the prod-like docker-compose stack, inserts a group with a synchronous wait,
## reads balances, then kills the active processor and asserts the standby takes
## over within the lease TTL. Self-contained (tears the stack down on exit); set
## KEEP_UP=1 to leave it running. Requires Docker + the compose plugin. M13 reuses it.
smoke:
	./scripts/smoke.sh

## tools: install pinned dev tooling into GOBIN.
tools:
	$(GO) install mvdan.cc/gofumpt@latest
