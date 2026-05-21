-- Wallet Transfer Service — schema
BEGIN;

-- =============================================================================
-- Wallets
-- =============================================================================
CREATE TABLE wallets (
    id          TEXT        PRIMARY KEY,
    balance     BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =============================================================================
-- Transfers
-- =============================================================================
CREATE TYPE transfer_status AS ENUM ('PENDING', 'PROCESSED', 'FAILED');
CREATE TYPE entry_direction AS ENUM ('DEBIT', 'CREDIT');

CREATE TABLE transfers (
    id               UUID            PRIMARY KEY,
    idempotency_key  TEXT            NOT NULL UNIQUE,
    payload_hash     CHAR(64)        NOT NULL,
    from_wallet_id   TEXT            NOT NULL REFERENCES wallets(id),
    to_wallet_id     TEXT            NOT NULL REFERENCES wallets(id),
    amount           BIGINT          NOT NULL CHECK (amount > 0),
    status           transfer_status NOT NULL DEFAULT 'PENDING',
    failure_reason   TEXT,
    created_at       TIMESTAMPTZ     NOT NULL DEFAULT now(),
    processed_at     TIMESTAMPTZ,
    CHECK (from_wallet_id <> to_wallet_id)
);

-- Partial index for the recovery worker (PROCESSED dominates; keep this small and hot).
CREATE INDEX ix_transfers_pending ON transfers (created_at) WHERE status = 'PENDING';

-- Indexes for transfer-history reads by wallet (out of scope but cheap to add).
CREATE INDEX ix_transfers_from ON transfers (from_wallet_id, created_at DESC);
CREATE INDEX ix_transfers_to   ON transfers (to_wallet_id,   created_at DESC);

-- =============================================================================
-- Ledger entries — append-only, double-entry, structurally exactly-once per transfer
-- =============================================================================
CREATE TABLE ledger_entries (
    id             BIGSERIAL       PRIMARY KEY,
    transfer_id    UUID            NOT NULL REFERENCES transfers(id),
    wallet_id      TEXT            NOT NULL REFERENCES wallets(id),
    direction      entry_direction NOT NULL,
    amount         BIGINT          NOT NULL CHECK (amount > 0),
    balance_after  BIGINT          NOT NULL CHECK (balance_after >= 0),
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, direction),   -- I1: exactly one DEBIT + one CREDIT per transfer
    UNIQUE (transfer_id, wallet_id)    -- a transfer touches each wallet at most once
);

CREATE INDEX ix_ledger_wallet ON ledger_entries (wallet_id, id);

COMMIT;
