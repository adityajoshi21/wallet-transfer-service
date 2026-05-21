# Wallet Transfer Service — Design Document

**Status:** Final, ready for implementation
**Scope:** Single-endpoint wallet-to-wallet transfer service. Single currency. Synchronous HTTP API.

---

## How to read this doc

- **§1–§3** — what we're building and what must always be true.
- **§7–§9** — how the system stays correct under retries and concurrency. These are the load-bearing sections.
- **The rest** — reference.

Companion docs:
- [`SYSTEM_INVARIANTS.md`](./SYSTEM_INVARIANTS.md) — full enforcement and test-coverage detail for every invariant.
- [`README.md`](./README.md) — how to run it and where the code lives.
---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Design Principles](#2-design-principles)
3. [Invariants](#3-invariants)
4. [Database Schema](#4-database-schema)
5. [API Contract](#5-api-contract)
6. [Expected Behavior](#6-expected-behavior)
7. [Transfer Algorithm](#7-transfer-algorithm)
8. [Concurrency & Race Conditions](#8-concurrency--race-conditions)
9. [Idempotency Strategy](#9-idempotency-strategy)
10. [Retry & Failure Model](#10-retry--failure-model)
11. [Side Effects](#11-side-effects)
12. [Recovery Worker](#12-recovery-worker)
13. [Architecture](#13-architecture)
14. [Observability](#14-observability)
15. [Testing Strategy](#15-testing-strategy)
16. [Out of Scope](#16-out-of-scope)
17. [Mapping to Evaluation Criteria](#17-mapping-to-evaluation-criteria)

---

## 1. Problem Statement

A service exposing `POST /transfers` that moves a positive integer `amount` from `fromWalletId` to `toWalletId`. Each request carries an `Idempotency-Key` header. The system must guarantee:

- **Idempotent request handling** — any retry of `(key, body)` produces at most one transfer, one pair of ledger entries, one balance mutation per wallet.
- **Double-entry ledger recording** — every successful transfer produces exactly two balancing entries (one DEBIT, one CREDIT).
- **Correct balance tracking** — wallet balances are accurate, fast to read, and reconcilable against the ledger.
- **Safe concurrent execution** — concurrent transfers on the same wallet never lose updates, never overdraw, never deadlock.
- **Exactly-once at the API** — the financial side effect of any logical transfer occurs exactly once, regardless of how many times the client retries.

A transfer is in one of three states; transitions are monotonic and terminal states are immutable:

```
              T2 commit (money moved)
   PENDING ───────────────────────────▶ PROCESSED
      │
      │       T2 commit (business rejection)
      └─────────────────────────────────▶ FAILED
```

**On atomicity.** Money movement is atomic: a single database transaction (T2) covers both ledger entries, both balance updates, and the terminal state transition. A separate transaction (T1) durably records intent before money moves; this is a deliberate split to support recovery of crashed callers, not a weakening of money-movement atomicity.

---

## 2. Design Principles

Four decisions every later section depends on.

**Money is `BIGINT` minor units.** `$10.50` is `1050`. Floating point cannot represent `0.10` exactly; repeated float arithmetic on money guarantees drift.

**The ledger is the source of truth; `wallets.balance` is a fast projection.** Pure derivation is O(history) per read; stored balance alone is unverifiable. Both: stored balance mutated only inside the same transaction as the ledger writes, with periodic reconciliation against the ledger.

**Money movement is one ACID database transaction.** Atomicity is delegated to the database because the database is the only component that can guarantee it cheaply. No application-level two-phase commit.

**Idempotency is structural, not behavioral.** The financial side effect is gated by a database `UNIQUE` constraint and a terminal-state row that is only ever *read* on replay. Duplicate requests cannot duplicate side effects because the writes are structurally unreachable a second time — not because the application checks for duplicates before writing.

---

## 3. Invariants

| # | Invariant | Primary enforcement |
|---|---|---|
| I1 | Per transfer, ledger entries sum to zero | `UNIQUE(transfer_id, direction)` + atomic T2 |
| I2 | Globally, system-wide debits = credits | I1 + atomic T2 |
| I3 | `wallets.balance` ≡ latest `ledger_entries.balance_after` | Balance + ledger updated in the same T2 commit |
| I4 | `wallets.balance >= 0` always | App-layer check under `FOR UPDATE` + DB `CHECK` |
| I5 | `ledger_entries.balance_after >= 0` | Computed under lock + DB `CHECK` |
| I6 | Each transfer has 0 (FAILED) or 2 (PROCESSED) ledger entries | T2 writes both legs or none |
| I7 | State monotonicity: `PENDING → {PROCESSED, FAILED}`, terminals immutable | Domain entity + conditional `UPDATE ... WHERE status='PENDING'` |
| I8 | At most one transfer per `idempotency_key` | `UNIQUE(idempotency_key)` |
| I9 | Same key + different payload is rejected | `payload_hash` comparison in REPLAY |
| I10 | `processed_at IS NOT NULL ⇔ status ∈ {PROCESSED, FAILED}` | Set in the same UPDATE that transitions to terminal |

See [`INVARIANTS.md`](./INVARIANTS.md) for each invariant's full enforcement detail and the specific test that would catch a regression.

---

## 4. Database Schema

```sql
CREATE TABLE wallets (
    id          TEXT        PRIMARY KEY,
    balance     BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    processed_at     TIMESTAMPTZ,                          -- set on PROCESSED or FAILED
    CHECK (from_wallet_id <> to_wallet_id)
);

CREATE INDEX ix_transfers_pending ON transfers (created_at) WHERE status = 'PENDING';
CREATE INDEX ix_transfers_from    ON transfers (from_wallet_id, created_at DESC);
CREATE INDEX ix_transfers_to      ON transfers (to_wallet_id,   created_at DESC);

CREATE TABLE ledger_entries (
    id             BIGSERIAL       PRIMARY KEY,
    transfer_id    UUID            NOT NULL REFERENCES transfers(id),
    wallet_id      TEXT            NOT NULL REFERENCES wallets(id),
    direction      entry_direction NOT NULL,
    amount         BIGINT          NOT NULL CHECK (amount > 0),
    balance_after  BIGINT          NOT NULL CHECK (balance_after >= 0),
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, direction),   -- structural I1: one DEBIT + one CREDIT per transfer
    UNIQUE (transfer_id, wallet_id)    -- a transfer touches each wallet at most once
);

CREATE INDEX ix_ledger_wallet ON ledger_entries (wallet_id, id);
```

### Key choices

- **`TEXT` wallet IDs** to match the brief's example.
- **`status` and `direction` as ENUMs** — the type system gives "only these values exist" for free; evolving the state machine is `ALTER TYPE ADD VALUE`.
- **No cross-column CHECK on `transfers`.** The transaction structure already guarantees `processed_at` and `status` move together; a CHECK that duplicates that logic fights schema evolution.
- **`idempotency_records` merged into `transfers`.** For one endpoint, the separate table would carry only `(key, transfer_id, payload_hash)` — fields that fit on `transfers` itself. See §9.1.
- **`balance_after`** on every ledger entry makes the ledger self-describing — point-in-time balance is O(1) per entry, reconciliation is a trivial join.
- **Direction + unsigned amount** (not signed amount) lets `UNIQUE(transfer_id, direction)` *structurally* prevent two-DEBITs-same-transfer bugs.
- **Partial index `ix_transfers_pending`** serves the recovery worker; PROCESSED dominates so a partial index stays small and hot.

---

## 5. API Contract

### `POST /transfers`

```http
POST /transfers
Content-Type: application/json
Idempotency-Key: abc123

{
  "fromWalletId": "wallet_1",
  "toWalletId":   "wallet_2",
  "amount":       100
}
```

The idempotency key lives in the `Idempotency-Key` header, not the body. This follows the Stripe convention and keeps payload identity (from/to/amount) cleanly separable from request identity (key) — the conceptual basis for `payload_hash` (§9).

**Validation (before any DB write):** `Idempotency-Key` present and non-empty (≤128 chars); `amount > 0`, integer; `fromWalletId != toWalletId`; both wallet IDs non-empty.

Wallet existence is enforced at T1 by the foreign key.

| Status | Condition |
|---|---|
| `201 Created` | New transfer terminated (PROCESSED or FAILED) |
| `201 Created` | Idempotent replay — same status as the original, per Stripe convention |
| `400 Bad Request` | Missing `Idempotency-Key`, malformed payload, non-positive amount, same wallet, unknown field |
| `404 Not Found` | Wallet does not exist |
| `409 Conflict` | Same idempotency key reused with a different payload |
| `503 Service Unavailable` | Transient (DB unavailable, lock timeout); no state persisted |

**FAILED returns 201, not 4xx.** Insufficient funds is a deterministic, replayable terminal outcome — not a malformed request. Reserving 4xx for client errors keeps replay semantics clean: a FAILED transfer replays identically to a PROCESSED one.

**Replay returns the same HTTP status as the original**, per Stripe. Clients cannot tell first-time-success from retry-success.

### Transfer object (response body)

```json
{
  "id":             "f1e8c5a2-9d3b-4e1f-a8c7-2b5d4e6f1a3c",
  "status":         "PROCESSED",
  "fromWalletId":   "wallet_1",
  "toWalletId":     "wallet_2",
  "amount":         100,
  "failureReason":  null,
  "createdAt":      "2026-05-20T10:00:00Z",
  "processedAt":    "2026-05-20T10:00:00Z"
}
```

### `GET /wallets/{id}` {Get wallet details}

Returns `{id, balance, createdAt, updatedAt}` or `404 WALLET_NOT_FOUND`. Pure read, no locks. Returns committed state at read time; a read concurrent with a transfer returns either pre- or post-transfer balance, never an intermediate value.

### `GET /wallets/{id}/transfers` {Get transfer history for a wallet}

Returns recent transfers involving the wallet (source or destination), most recent first.

Query params: `limit` (1–100, default 50), `before` (RFC3339Nano cursor for keyset pagination).

```json
{
  "walletId": "wallet_1",
  "count":    2,
  "transfers": [ /* transfer objects */ ],
  "nextBefore": "2026-05-20T10:00:00Z"   // present only if more may exist
}
```

Includes transfers in all states (PENDING, PROCESSED, FAILED) — a client needs to see "your transfer was rejected" not just "your money didn't move." Empty history returns `200` with empty array, distinct from `404`.

---

## 6. Expected Behavior

For any well-formed `POST /transfers`:

1. **Exactly one terminal transfer record** exists for the given key — either `PROCESSED` or `FAILED`.
2. **Exactly two ledger entries** exist for PROCESSED; **zero** for FAILED.
3. **Balances change by `±amount`** iff PROCESSED. No observable intermediate state.
4. **Every retry with the same key + same payload returns the same response**, forever.
5. **No wallet balance is ever negative.**
6. **No deadlocks under concurrent load.**

Business rejection is a correct, deterministic, terminal `FAILED` outcome — not an error.

---

## 7. Transfer Algorithm

Two transactions. **T1** reserves the idempotency key. **T2** moves money atomically. **REPLAY** handles duplicates; when it finds a `PENDING` row it runs T2 inline.

### T1 — Reserve the idempotency key

```
BEGIN
  INSERT INTO transfers (id, idempotency_key, payload_hash, from, to, amount,
                         status = 'PENDING')
  -- On UNIQUE(idempotency_key) violation → REPLAY
  -- On FK violation on from/to wallet → 404 (no transfer row created)
COMMIT
```

After T1 commits, the key is durably reserved as PENDING. No money has moved.

### T2 — Move the money atomically

Everything below commits together or rolls back together. T2 is also entered by REPLAY when an existing transfer is PENDING — in that case, the SELECT FOR UPDATE on the transfer was already done by REPLAY in the same transaction, and execution continues from step 2.

```
BEGIN
  -- 1. Lock the transfer row; bail if not PENDING
  SELECT ... FROM transfers WHERE id = :id FOR UPDATE;
  IF status <> 'PENDING' THEN GOTO REPLAY; END IF;

  -- 2. Lock both wallets in ASCENDING ID order
  SELECT id, balance FROM wallets
    WHERE id IN (:from, :to) ORDER BY id FOR UPDATE;

  -- 3a. Insufficient funds → FAILED (terminal, no money movement)
  IF from.balance < :amount THEN
    UPDATE transfers SET status = 'FAILED',
                         failure_reason = 'INSUFFICIENT_FUNDS',
                         processed_at = now()
      WHERE id = :id AND status = 'PENDING';
    COMMIT; RETURN FAILED;
  END IF;

  -- 3b. Sufficient funds → PROCESSED
  INSERT INTO ledger_entries (transfer_id, wallet_id, direction, amount, balance_after) VALUES
    (:id, :from, 'DEBIT',  :amount, from.balance - :amount),
    (:id, :to,   'CREDIT', :amount, to.balance   + :amount);

  UPDATE wallets SET balance = balance - :amount WHERE id = :from;
  UPDATE wallets SET balance = balance + :amount WHERE id = :to;

  UPDATE transfers SET status = 'PROCESSED', processed_at = now()
    WHERE id = :id AND status = 'PENDING';
  -- rows_affected must be 1; if 0 → state anomaly, rollback, REPLAY

  COMMIT; RETURN PROCESSED
```

### REPLAY — pure read, or inline T2 on stranded PENDING

Entered on `UNIQUE(idempotency_key)` violation in T1, or when T2's status check finds the row already terminal.

```
BEGIN
  existing := SELECT * FROM transfers WHERE idempotency_key = :key FOR UPDATE;

  IF existing.payload_hash <> incoming_payload_hash:
      ROLLBACK; RETURN 409 IDEMPOTENCY_KEY_REUSE

  IF existing.status IN ('PROCESSED', 'FAILED'):
      ROLLBACK; RETURN 201 with existing transfer       -- pure read; safe

  IF existing.status = 'PENDING':
      -- T1 committed but T2 never ran (caller crashed, or another instance
      -- was mid-T2 and our SELECT FOR UPDATE blocked until they committed).
      -- We hold the row lock; continue THIS SAME TRANSACTION as T2 from step 2.
      GOTO T2 step 2
COMMIT
```

The retry handler's transaction *is* T2 when it finds PENDING. No second transaction; the SELECT FOR UPDATE lock carries through into the wallet locks and ledger writes. This eliminates the client poll loop — retries arriving while the original is in flight (or after the original crashed before T2) simply execute T2 themselves.

---

## 8. Concurrency & Race Conditions

Four races, four defenses.

| Race | Defense |
|---|---|
| Two identical requests, same key, simultaneously | `UNIQUE(idempotency_key)` serializes T1 at the DB. One INSERT commits; the other gets 23505 and goes to REPLAY. The unique index *is* the mutex. |
| Two different transfers draining the same wallet (lost-update / overdraft) | `SELECT ... FOR UPDATE` on the wallet row. Second transaction blocks until first commits, then re-reads balance under lock and re-evaluates funds check. `CHECK (balance >= 0)` as backstop. |
| Crossing transfers A→B and B→A (deadlock) | Always lock wallets in **ascending ID order** in T2. Both transactions request locks in the same global order; one waits cleanly. |
| Retry-handler T2 vs. recovery-worker T2 on same PENDING row | Both contend for SELECT FOR UPDATE on the transfer row. One wins and runs T2; the other unblocks, sees terminal status, returns the cached outcome. Recovery worker uses `FOR UPDATE SKIP LOCKED` to avoid even queuing for rows the retry handler is actively working. |

### Why these choices

**Pessimistic row-level locks at READ COMMITTED, with deterministic lock order.**

Considered and rejected:
- **Optimistic locking (`version` column).** Composability is awkward for multi-wallet operations; `FOR UPDATE` already provides what optimistic would. Stacking them adds a column with no incremental safety.
- **SERIALIZABLE isolation.** Correct, but pushes failure handling to the app via 40001 retries; harder to reason about which invariant is preserved where.
- **Advisory locks.** Decouples locking from row identity; possible deadlocks if the key scheme is wrong, and audits get harder.

READ COMMITTED is sufficient because every contended read in T2 is an explicit `FOR UPDATE`.

---

## 9. Idempotency Strategy

**Storage.** `transfers.idempotency_key` with a `UNIQUE` constraint — see §9.1 for why we don't have a separate table.

**Duplicate detection at write time.** The UNIQUE-constraint violation on T1's INSERT is the detection. Not a pre-SELECT — a check-then-insert has a TOCTOU race; relying on the constraint does not.

**Original-result return.** The terminal transfer row *is* the stored result. PROCESSED/FAILED are immutable, so REPLAY reconstructs the exact original response by reading the row.

**Two independent layers prevent duplicate side effects:**

1. `UNIQUE(idempotency_key)` ensures exactly one request ever proceeds past T1 to execute T2. All others can only *read*.
2. `UNIQUE(transfer_id, direction)` on the ledger ensures even an internal re-execution (retry-handler-runs-T2, recovery worker) cannot post a transfer's legs twice.

The financial side effect is therefore structurally reachable at most once. Take either constraint away and a single bug above it could cause double-spend; with both in place, two simultaneous bugs would be needed.

**`payload_hash`** is SHA-256 over canonical JSON of *payload-identity fields only* — `fromWalletId`, `toWalletId`, `amount`. The `idempotency_key` itself is the *lookup*, not part of the identity check. Canonicalization: keys sorted, no whitespace, integers as digit strings, no nulls, UTF-8. A mismatch in REPLAY returns 409 — never silently honors the request.

**Not cached:** pre-T1 validation errors (400/404). No transfer row is created, so a retry re-validates against current state. Validation outcomes aren't stable across world changes (e.g., a wallet is created later), so caching them would be wrong.

### 9.1 Why no separate `idempotency_records` or `transfer_events` tables

The brainstormed about it And the simple approach sufficed: **the data they would carry already lives in this schema with stronger structural guarantees.**

**`idempotency_records`** would carry `(key, transfer_id, payload_hash, response_status, response_body)`. The first three fields fit cleanly on `transfers` itself. The response fields are unnecessary because the response is *derivable from the transfer's terminal state* — caching it would be storing information we can already reconstruct. `UNIQUE(transfers.idempotency_key)` provides the same write-time serialization a `PRIMARY KEY` on a separate table would.

**`transfer_events`** would record state transitions. Our state machine has at most one transition per transfer (PENDING → terminal); the transition is captured by `(status, failure_reason, processed_at)` on the row itself. Terminal-state immutability (I7) is enforced by a conditional UPDATE. Money-movement audit comes from the append-only ledger.

What we keep without the tables: structural enforcement of every audit invariant the tables would have protected. Terminal states are immutable (conditional UPDATE). State transitions can't be silently lost (atomic T2). Reversals are visible (new compensating ledger entries, never UPDATE). Money movement is fully auditable (`ledger_entries` ordered by `id`).

This trade-off would flip with a more complex state machine (multi-step settlement, holds/releases, multi-actor authorization) where the audit signal is no longer derivable from current state alone. For this simple wallet transfer, the extra tables would be dead weight.

---

## 10. Retry & Failure Model

| # | Failure point | What persisted | Outcome on retry (same key) |
|---|---|---|---|
| 1 | Network drops before request reaches server | nothing | First-time execution |
| 2 | Crash after T1 commit, before T2 | transfer = PENDING | Retry acquires lock, sees PENDING, runs T2 inline |
| 3 | Crash during T2 (before COMMIT) | T2 rolled back | Transfer still PENDING → retry runs T2 |
| 4 | Crash after T2 commit, before HTTP response | transfer = terminal | Retry → REPLAY → original response |
| 5 | Two identical requests race | one wins T1 INSERT | Loser → REPLAY → winner's outcome |
| 6 | Concurrent drains on one wallet | row lock serializes | No overdraft, no lost update |
| 7 | Crossing transfers A→B / B→A | deterministic lock order | One waits, no deadlock |
| 8 | Insufficient funds | transfer = FAILED | Retry → same FAILED, idempotent |
| 9 | Key reused with different payload | original untouched | 409, no side effect |
| 10 | DB unavailable mid-request | nothing committed | 503 → retry with backoff |
| 11 | Client crashes and never retries | transfer = PENDING (stranded) | Recovery worker runs T2 |
| 12 | Pre-T1 validation failure (400/404) | nothing persisted | Re-validates; outcome may differ if world changed |

**Structural reason every row is safe.** The only step that moves money is one atomic transaction (T2), gated by an idempotency key whose terminal outcome is immutable and only ever *read* on replay. A crash either happens before T2 (nothing moved, retry runs it) or after it (it fully completed, retry just reads it). There is no halfway because the database does not permit one.

**Client retry contract.** Always retry with the same key (a different key creates a new transfer). Retry only on retryable signals (network timeout, 503, connection reset) — never on 400/409. Use exponential backoff with jitter.

---

## 11. Side Effects

| Side effect | When | Mutability |
|---|---|---|
| `transfers` row inserted (PENDING) | T1 commit | Key permanently reserved |
| `wallets.balance` mutated (both rows) | T2 commit, atomically | Mutable per transfer; reversal requires a compensating transfer (ledger stays the audit trail) |
| Two `ledger_entries` inserted | T2 commit, atomically | **Immutable.** Corrections are new compensating entries |
| `transfers.status` → terminal | T2 commit | **Immutable.** Terminal states never change |

No external side effects (notifications, webhooks) on the hot path. If added later they must be emitted from a transactional outbox written inside T2.

---

## 12. Recovery Worker

A background worker for the long-tail case: client crashed and will never retry.

```sql
SELECT id FROM transfers
WHERE status = 'PENDING' AND created_at < now() - INTERVAL :threshold
ORDER BY created_at LIMIT 100
FOR UPDATE SKIP LOCKED;
```

For each row, run T2. Re-running is safe: T2 re-checks status under FOR UPDATE; `UNIQUE(transfer_id, direction)` makes a duplicate ledger post impossible; `SKIP LOCKED` avoids contention with the retry handler.

The threshold is configured at **30 seconds** in `cmd/server/main.go`. Lower = faster recovery, more contention with in-flight retries; higher = slower recovery, less contention. The retry handler running T2 inline means this is a safety net, not the hot path.

---

## 13. Architecture

Four layers, matching the brief's terminology. Outer layers depend inward; the domain depends on nothing.

```
handler  →  service  →  repository
                ↓
             domain
```

### Boundary rules

- **Transaction boundaries live in the service layer**, not repositories. One use-case invocation = one well-defined set of DB transactions. Repositories never COMMIT on their own; they accept `pgx.Tx` and operate inside it.
- **The state machine lives in the `Transfer` domain entity.** `MarkProcessed` and `MarkFailed` reject non-PENDING source states. Illegal transitions are impossible to express, not merely discouraged. SQL UPDATEs to `status` go through the entity, never directly.
- **The domain layer is pure.** No SQL, no clock, no framework. Unit-testable exhaustively with zero infrastructure — exactly the code path that must never have a bug.
- **Dependency inversion.** The service layer declares repository interfaces; the repository layer implements them.

---

## 14. Observability

**Structured logging is built.** `log/slog` via `internal/logger`, JSON output by default (text for local dev via `WALLET_LOG_FORMAT=text`), level configurable via `WALLET_LOG_LEVEL`. Loggers are component-tagged via `logger.Component("service.transfer")`. Per-request HTTP-level correlation via middleware is intentionally not built — domain fields like `transfer_id` are the correlation key for this domain, because retries of the same transfer share it.

**Metrics and alerts are designed but not built** (see §16). Production would emit: outcome rate by status, T1/T2 latency, lock-wait time, recovery-worker queue depth, reconciliation drift. Alerts on drift > 0 (paging), 503 rate, and FAILED rate spikes.

---

## 15. Testing Strategy

Tests are structured by invariant. Every concurrency test ends with `assertInvariants()` in `tests/helpers_test.go`, which re-checks I1, I3, I4, I6 against the live DB. If an invariant ever fails, the test fails — regardless of what the test was specifically asserting.

**Unit tests (domain, no DB):** Wallet debit/credit, Transfer state transitions (every legal and illegal one), hash determinism and scope.

**Integration tests (real Postgres):** happy path, insufficient funds, replay same payload, replay different payload (409), wallet-not-found, validation errors, read endpoints.

**Concurrency tests:**
- **I4 overdraft prevention** — 10 parallel transfers of 100 from a wallet at 500. Exactly 5 PROCESSED, 5 FAILED, final balance 0.
- **I8 idempotency race** — 50 parallel calls with the same key + body. Exactly 1 transfer, 1 ledger pair, identical responses.
- **Crossing transfers** — 100 alternating A→B/B→A in parallel. No deadlocks.
- **Recovery worker** — stranded PENDING transfer driven to terminal.
- **REPLAY-runs-T2** — directly inserted PENDING transfer driven to terminal by a retry.

**Regression tests** (`tests/regression_test.go`) — direct tests for each safety-net constraint: I7 conditional UPDATE returns 0 rows on terminal states; ledger UNIQUE rejects double DEBIT/CREDIT; idempotency-key UNIQUE rejects duplicate rows. Defense-in-depth coverage that would catch a regression even if behavioral tests still passed.

**HTTP tests** for header-based idempotency: happy path, missing header (400), key-in-body rejection, replay via header, 409 on payload mismatch, read endpoints.

**TDD discipline.** The I4 and I8 concurrency tests are the highest-leverage tests — write them before any business logic.

---

## 16. Out of Scope

The four scope decisions a reviewer might wonder about:

- **No `transfer_events` audit table.** Rationale in §9.1. Would become correct if the state machine grew beyond one transition.
- **No transactional outbox.** No external consumers in this scope. Adding webhooks/notifications later requires it.
- **No metrics emission or alerting.** Designed in §14, not built. Structured logging *is* built.
- **No `GET /transfers/{id}`.** Optional Enhancement in the brief; not implemented. `GET /wallets/{id}` and `GET /wallets/{id}/transfers` are implemented.
- **No Comprehensive reconciliation and drift monitoring against the ledger is out of scope; a scheduler for that would be a production addition.
- **No Similarly, error handling and logging has been minimal and at times leak implementation details, but a production system would have more nuanced.

Also out of scope : wallet creation endpoint, multi-currency, auth, rate limiting, archival, request signing.

---

## 17. Mapping to Evaluation Criteria

| Axis | Where addressed |
|---|---|
| **Database Design** — schema clarity, constraints, integrity, indexes | §4 (schema), §3 (every invariant ↔ structural enforcement) |
| **Transaction Strategy** — boundaries, locks, race handling | §7 (T1/T2/REPLAY), §8 (four races, four defenses) |
| **Idempotency Strategy** — correctness under retries, dedup | §9 (structural via UNIQUE), §10 (failure model) |
| **Code Structure** — clean layering, separation of concerns | §13 (four layers, owned transaction boundaries, pure domain) |
| **Testing** — meaningful coverage, correctness validation | §15 (invariant-driven, concurrency-first, regression coverage) |