# Architecture Decision Records

Each ADR records one deviation from, or refinement of, `docs/architecture-spec.md`
or `docs/plan.md`. The rule the whole repo relies on: **the spec plus this ADR log
is always the complete current truth.** If code diverges from the spec and no ADR
explains it, that is a bug in either the code or the log.

## Conventions

- One file per decision, numbered `NNNN-kebab-title.md`, ~half a page.
- Copy `template.md` for a new one. Take the next free number; **never renumber an
  existing ADR** (other documents and commits reference them by number).
- Sections: Status / Context / Decision / Consequences (Rationale and Invariants
  preserved are optional where they clarify).
- Status is `accepted`, `superseded by ADR-NNNN`, or `proposed`.
- Write the ADR in the same commit series as the change it justifies.

## Index

- [0001 — Custom migration runner](0001-custom-migration-runner.md) — a purpose-built
  ~150-line runner in `internal/migrate` instead of a third-party migration tool.
- [0002 — Bidirectional LISTEN/NOTIFY](0002-bidirectional-listen-notify.md) — work
  doorbell + outcome fan-out as the sanctioned low-latency path.
- [0003 — Code-first OpenAPI via Huma v2](0003-code-first-openapi-huma.md) — the
  server is the authority; the committed spec is derived from real types.
