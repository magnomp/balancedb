-- 0002_operation_edits.sql — operation editing (ADR-0010; feature spec
-- .compozy/tasks/operation-editing/_spec.md, Part II → Data Models).
--
-- Additive and forward-only: new nullable/defaulted columns on operations, one
-- append-only side table, one partial index, three CHECKs and a config knob.
-- Nothing here rewrites an existing row — the defaults are constants, so Postgres
-- records them in the catalog and existing tuples keep their xmin.
--
-- An edit is a registration row in operations with edit_of = <target id>; it
-- carries the full proposed state and sits in the same PENDING work queue. The
-- leader applies it by overwriting the target's current columns under a revision
-- CAS and appending the superseded state to operation_revisions. Names are
-- unqualified: the runner sets search_path first (plan §0).

ALTER TABLE operations
  ADD COLUMN edit_of           BIGINT NULL REFERENCES operations(id),
  ADD COLUMN expected_revision INT    NULL,
  ADD COLUMN revision          INT    NOT NULL DEFAULT 1,
  ADD COLUMN revised_at        TIMESTAMPTZ NULL,   -- when the current revision became current (NULL = never edited)
  ADD CONSTRAINT ops_edit_not_reversal CHECK (edit_of IS NULL OR reversal_of IS NULL),
  ADD CONSTRAINT ops_edit_not_self     CHECK (edit_of IS NULL OR edit_of <> id),
  ADD CONSTRAINT ops_expected_rev_pos  CHECK (expected_revision IS NULL OR expected_revision >= 1);

-- "which edits target X": serves the history endpoint's pending and rejected lists
-- (applied edits are reachable through operation_revisions.superseded_by). The
-- processor never uses it. Covers every edit row, never regular operations, so it
-- stays small; the FK needs no index because operation ids are never updated/deleted.
CREATE INDEX idx_ops_edit_of ON operations (edit_of) WHERE edit_of IS NOT NULL;

-- Append-only: one row per SUPERSEDED state. The current state lives on operations.
CREATE TABLE operation_revisions (
  operation_id  BIGINT NOT NULL REFERENCES operations(id),
  revision      INT    NOT NULL,                 -- the revision being superseded
  account_id    BIGINT NOT NULL REFERENCES accounts(id),
  amount        BIGINT NOT NULL,
  effective_at  TIMESTAMPTZ NOT NULL,
  recorded_at   TIMESTAMPTZ NOT NULL,            -- when this revision became current
  superseded_by BIGINT NOT NULL REFERENCES operations(id),  -- the APPLIED edit row
  superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (operation_id, revision)
);

-- Behavioral knob, hot-reloaded (plan §0 split): FALSE keeps a cell on the
-- original reversal-only contract.
ALTER TABLE config ADD COLUMN allow_edits BOOLEAN NOT NULL DEFAULT TRUE;
