-- 0003_operation_deletes.sql — operation deletion (ADR-0011; feature spec
-- .compozy/tasks/operation-deletion/_spec.md, Part II → Data Models).
--
-- Additive and forward-only: three defaulted/nullable columns on operations, two
-- CHECKs and a config knob. Nothing here rewrites an existing row — the defaults
-- are constants, so Postgres records them in the catalog and existing tuples
-- keep their xmin. No new table: the deletion event is two columns on the row it
-- concerns, and the registration row itself is the matchable record (reachable
-- through idx_ops_edit_of).
--
-- A delete is an edit-class registration (edit_of set) with is_delete = TRUE. The
-- leader applies it by flipping the target CONFIRMED → DELETED under the revision
-- CAS; the target's last values stay on its row. Nothing is ever physically
-- deleted. Names are unqualified: the runner sets search_path first (plan §0).

ALTER TABLE operations
  ADD COLUMN is_delete  BOOLEAN     NOT NULL DEFAULT FALSE,  -- edit-class row that deletes its target
  ADD COLUMN deleted_by BIGINT      NULL REFERENCES operations(id),  -- regular row: the APPLIED delete registration
  ADD COLUMN deleted_at TIMESTAMPTZ NULL,                    -- regular row: when it became DELETED
  ADD CONSTRAINT ops_delete_is_edit CHECK (NOT is_delete OR edit_of IS NOT NULL),
  ADD CONSTRAINT ops_deleted_pair   CHECK ((deleted_by IS NULL) = (deleted_at IS NULL));

-- Behavioral knob, hot-reloaded (plan §0 split), independent of allow_edits.
ALTER TABLE config ADD COLUMN allow_deletes BOOLEAN NOT NULL DEFAULT TRUE;
