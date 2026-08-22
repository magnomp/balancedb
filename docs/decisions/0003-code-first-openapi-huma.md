# ADR-0003 — Code-first OpenAPI via Huma v2 (server is the authority)

Status: accepted (operator decision, 2026-08-21). Supersedes the earlier spec-first draft of this ADR.

## Context
The operator requires a Swagger/OpenAPI-documented HTTP API and prefers the server as the authoritative layer with the spec derived from it (their working model from Node). Options considered:
- swaggo/swag (code-first from comment annotations): rejected — comments are unchecked against behavior; worst drift profile.
- oapi-codegen (spec-first): rejected by operator preference; also a 3-step change dance (yaml → generate → implement).
- Huma v2 (code-first from real types): chosen.

## Decision
`internal/api` is built on Huma v2 over chi/stdlib mux. Endpoints are typed operations (`func(ctx, *Input) (*Output, error)`); Huma reflects over the actual request/response structs to produce OpenAPI 3.1 + JSON Schema, performs request validation, returns RFC 7807 errors, and serves `/docs` and `/openapi.yaml`.

Contract-as-artifact: `make openapi` exports the generated spec to `api/openapi.yaml`, which is committed. CI regenerates and fails on diff (every PR shows API changes as a reviewable yaml delta) and runs `oasdiff` to flag breaking changes.

## Rationale
- Drift is structurally impossible: the struct that generates the schema is the struct that decodes the request. This is stronger than annotation- or decorator-derived specs.
- One-step changes: edit the operation's types; docs, validation, and spec follow.
- The committed yaml preserves spec-first's benefits (review, client codegen, breaking-change detection) without being a second source of truth.

## Consequences
- Toolchain: Go 1.25+ (Huma requirement). Dockerfile/devcontainer images updated accordingly.
- The "no framework" principle (plan §0) is deliberately bent at the API boundary only: Huma replaces hand-rolled validation/serialization there; the processor and all persistence code remain framework-free. Any expansion of Huma beyond `internal/api` requires a new ADR.
- Amount fields are int64 in structs → `integer/int64` schemas; JSON bodies with fractional amounts fail decoding by type. Minor-units and JS-precision warnings go in field descriptions.
- Error model at the API boundary is RFC 7807; BalanceDB's machine-readable rejection details embed as typed extension fields.
