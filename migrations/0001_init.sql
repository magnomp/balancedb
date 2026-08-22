-- 0001_init.sql — initial cell schema (spec §5.2).
--
-- The full per-cell DDL: accounts, transactions, operations, balance_snapshots,
-- leader_lease, config, and all indexes, plus the single-row seed for
-- leader_lease and config (INSERT ... ON CONFLICT DO NOTHING, ADR-0001).
--
-- All names are unqualified: the migration runner sets search_path to the target
-- schema before applying this file, so the same DDL installs into any configured
-- schema (plan §0). Forward-only; never edit this file once applied.

CREATE TABLE accounts (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  owner_id          BIGINT NOT NULL,           -- sharding key; all group legs share one owner
  external_id       TEXT   NOT NULL,
  min_balance       BIGINT NULL,               -- NULL = unbounded
  max_balance       BIGINT NULL,
  confirmed_balance BIGINT NOT NULL DEFAULT 0, -- FINAL balance (sum of all CONFIRMED ops)
  version           BIGINT NOT NULL DEFAULT 0, -- optimistic lock (processor vs API races)
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (owner_id, external_id)
);

CREATE TABLE transactions (          -- group records; ONLY for groups (>= 2 operations)
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  idempotency_key UUID  NOT NULL UNIQUE,
  payload_hash    BYTEA NOT NULL,
  op_count        INT   NOT NULL,
  status          TEXT  NOT NULL DEFAULT 'PENDING',  -- PENDING | COMMITTED | REJECTED
  reject_reason   TEXT  NULL,                        -- LIMIT_VIOLATED + detail
  registered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  decided_at      TIMESTAMPTZ NULL
);

CREATE TABLE operations (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
                      -- registration id: global insertion order; ordering tiebreaker
  account_id          BIGINT NOT NULL REFERENCES accounts(id),
  amount              BIGINT NOT NULL CHECK (amount <> 0),
  effective_at        TIMESTAMPTZ NOT NULL,   -- client-supplied; past AND future allowed
  transaction_id      BIGINT NULL REFERENCES transactions(id),  -- NULL for singles
  reversal_of         BIGINT NULL REFERENCES operations(id),
  status              TEXT NOT NULL DEFAULT 'PENDING',
                      -- PENDING | CONFIRMED | INVALID   (no intermediate states)
  invalidation_reason TEXT NULL,              -- LIMIT_VIOLATED + offending account/limit
  idempotency_key     UUID  NULL,             -- singles only (groups: on transactions)
  payload_hash        BYTEA NULL,             -- singles only
  registered_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  confirmed_at        TIMESTAMPTZ NULL
);

CREATE INDEX idx_ops_work     ON operations (id) WHERE status = 'PENDING';   -- work queue
CREATE INDEX idx_ops_timeline ON operations (account_id, effective_at, id);  -- reads
CREATE INDEX idx_ops_tx       ON operations (transaction_id) WHERE transaction_id IS NOT NULL;

CREATE UNIQUE INDEX idx_ops_reversal ON operations (reversal_of)
  WHERE reversal_of IS NOT NULL AND status <> 'INVALID';  -- one live reversal per op;
                                                          -- a rejected reversal allows retry

CREATE UNIQUE INDEX idx_ops_idem ON operations (idempotency_key)
  WHERE idempotency_key IS NOT NULL;

CREATE TABLE balance_snapshots (
  account_id BIGINT NOT NULL,
  day        DATE   NOT NULL,     -- UTC bucket of effective_at (timezone fixed forever)
  balance    BIGINT NOT NULL,     -- CUMULATIVE balance at end of that day
  PRIMARY KEY (account_id, day)
);

CREATE TABLE leader_lease (       -- single row, created at cell setup
  singleton   BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  owner       UUID NULL,          -- processor instance UUID (generated at boot)
  lease_until TIMESTAMPTZ NULL
);

CREATE TABLE config (             -- single row; hot-reloaded every cycle
  singleton         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  lease_ttl_ms      INT NOT NULL DEFAULT 15000,
  loop_interval_ms  INT NOT NULL DEFAULT 1000,
  batch_size        INT NOT NULL DEFAULT 200,   -- decisions per DB commit (§8.4)
  max_group_size    INT NOT NULL DEFAULT 10,
  api_max_wait_ms   INT NOT NULL DEFAULT 30000
);

-- Seed the two singleton rows. Column defaults fill every behavioral value, so a
-- fresh cell has a usable leader_lease and config without further wiring. Both are
-- ON CONFLICT DO NOTHING so re-running (or a concurrent migrator) is a no-op.
INSERT INTO leader_lease (singleton) VALUES (TRUE) ON CONFLICT DO NOTHING;
INSERT INTO config       (singleton) VALUES (TRUE) ON CONFLICT DO NOTHING;
