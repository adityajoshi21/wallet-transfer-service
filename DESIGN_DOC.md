# Wallet Transfer Service — Design Document

**Status:** Final, ready for implementation
**Scope:** Single-endpoint wallet-to-wallet transfer service. Single currency. Synchronous HTTP API.

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Design Principles](#2-design-principles)
3. [Invariants & Enforcement](#3-invariants--enforcement)
4. [Database Schema](#4-database-schema)
5. [API Contract](#5-api-contract)
6. [Expected Behavior](#6-expected-behavior)
7. [Transfer Algorithm](#7-transfer-algorithm)
8. [Concurrency & Race Conditions](#8-concurrency--race-conditions)
9. [Idempotency Strategy](#9-idempotency-strategy)
10. [Retry & Failure Model](#10-retry--failure-model)
11. [Consistency Expectations](#11-consistency-expectations)
12. [Side Effects](#12-side-effects)
13. [Recovery Worker](#13-recovery-worker)
14. [Architecture](#14-architecture)
15. [Observability Expectations](#15-observability-expectations)
16. [Testing Strategy](#16-testing-strategy)
17. [Out of Scope / Future Work](#17-out-of-scope--future-work)
18. [Mapping to Assignment Evaluation Criteria](#18-mapping-to-assignment-evaluation-criteria)

---

## 1. Problem Statement

A service exposing `POST /transfers` that moves a positive integer `amount` from `fromWalletId` to `toWalletId`. Each request carries an `idempotencyKey`. The system must guarantee:

- **Idempotent request handling** — any retry of `(key, body)` produces at most one transfer, one pair of ledger entries, one balance mutation per wallet.
- **Double-entry ledger recording** — every successful transfer produces exactly two balancing entries (one DEBIT, one CREDIT).
- **Correct balance tracking** — wallet balances are accurate, fast to read, and reconcilable against the ledger.
- **Safe concurrent execution** — concurrent transfers on the same wallet never lose updates, never overdraw, never deadlock.
- **Exactly-once semantics at the API level** — the financial side effect of any logical transfer occurs exactly once, regardless of how many times the client retries.

A transfer is in one of three states; transitions are monotonic and terminal states are immutable:

```
              T2 commit (money moved)
   PENDING ───────────────────────────▶ PROCESSED
      │
      │       T2 commit (business rejection)
      └─────────────────────────────────▶ FAILED
```

**Note on "atomic" execution.** Money movement is atomic: a single database transaction (T2) covers both ledger entries, both balance updates, and the terminal state transition. They commit together or roll back together. A separate transaction (T1) durably records intent before money moves; this is a deliberate split to support recovery of crashed callers, not a weakening of money-movement atomicity. See §7 and §10.

---

## 2. Design Principles

Four decisions stated up front because every later section depends on them.

**Money is stored as `BIGINT` minor units.** A `$10.50` balance is `1050`. Floating point cannot represent `0.10` exactly; repeated float arithmetic on money guarantees drift. Forbidden.

**The ledger is the source of truth; `wallets.balance` is a fast projection.** Pure derivation (`SUM(ledger)`) is always correct but O(history) per read. A stored balance alone is O(1) but unverifiable. The hybrid — stored balance mutated only inside the same transaction as the ledger writes, with periodic reconciliation against the ledger — is correct *and* fast.

**Money movement is one ACID database transaction.** Atomicity is delegated to the database because the database is the only component that can guarantee it cheaply. We do not attempt application-level two-phase commit.

**Idempotency is structural, not behavioral.** The financial side effect is gated by a database `UNIQUE` constraint and a terminal-state row that is only ever *read* on replay. Duplicate requests cannot duplicate side effects because the writes are structurally unreachable a second time — not because the application checks for duplicates before writing.

---

## 3. Invariants & Enforcement

The spine of the design. Every property below is enforced *structurally* at write time and *verifiable* at runtime.

| # | Invariant | Write-time enforcement | Runtime verification |
|---|---|---|---|
| I1 | Per transfer, ledger entries sum to zero | `UNIQUE(transfer_id, direction)` ensures one DEBIT + one CREDIT; app writes both with the same `amount` inside T2 | Property test on every committed transfer |
| I2 | Globally, system-wide debits = credits | I1 + atomic T2 commit | Reconciliation job; alert on drift |
| I3 | `wallets.balance` = ledger-derived balance per wallet | Balance and ledger updated in the same T2 commit, while holding the wallet's `FOR UPDATE` lock | Reconciliation compares `wallets.balance` to latest `ledger_entries.balance_after` |
| I4 | `wallets.balance >= 0` always | App-layer funds check under `FOR UPDATE`; DB `CHECK (balance >= 0)` as backstop | DB constraint — cannot be violated at write time |
| I5 | `ledger_entries.balance_after >= 0` | Computed inside T2 from the locked balance; DB `CHECK (balance_after >= 0)` | DB constraint |
| I6 | Each transfer has exactly 0 (FAILED) or 2 (PROCESSED) ledger entries | T2 writes both legs or neither (atomic); FAILED path writes none | Reconciliation query |
| I7 | State monotonicity: `PENDING → {PROCESSED, FAILED}`; terminals immutable | Domain entity methods + conditional `UPDATE ... WHERE status='PENDING'`; `rows_affected=0` → REPLAY | Logged; alerted on |
| I8 | At most one transfer per `idempotency_key` | `UNIQUE(idempotency_key)` | DB constraint |
| I9 | Same key + different payload is rejected, never honored | `payload_hash` comparison in REPLAY path | Test |
| I10 | `processed_at IS NOT NULL ⇔ status ∈ {PROCESSED, FAILED}` (set on both terminal paths) | Set in the same UPDATE that transitions to terminal | Invariant test |

Every "what if X happens?" question maps to a row in this table. The answer is the enforcement column.

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
    direction      entry_direction NOT NULL,                       -- brief calls this "type"; semantically identical
    amount         BIGINT          NOT NULL CHECK (amount > 0),
    balance_after  BIGINT          NOT NULL CHECK (balance_after >= 0),
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, direction),   -- structural I1: one DEBIT + one CREDIT per transfer
    UNIQUE (transfer_id, wallet_id)    -- a transfer touches each wallet at most once
);

CREATE INDEX ix_ledger_wallet ON ledger_entries (wallet_id, id);
```

### Schema decision notes

- **Wallet IDs are `TEXT`** to match the brief's example (`"wallet_1"`).
- **Money is `BIGINT` minor units.**
- **`status` and `direction` are Postgres ENUMs**, not VARCHAR + CHECK. The type system gives "only these values exist" for free; evolving the state machine is `ALTER TYPE ADD VALUE` instead of a CHECK migration.
- **Cross-column CHECK constraints on `transfers` are deliberately omitted.** The transaction structure already guarantees `processed_at` and `status` move together; a CHECK that duplicates that logic fights schema evolution.
- **`idempotency_records` is merged into `transfers`.** The brief suggests it as a separate table; we merge for the following reasons:
  1. For a single endpoint, the idempotency key maps one-to-one to a single transfer aggregate. A separate table would carry only `(key, transfer_id, payload_hash)` — fields that fit cleanly on `transfers` itself.
  2. `UNIQUE(transfers.idempotency_key)` is structurally identical to `PRIMARY KEY` on a separate `idempotency_records(key)` table — same write-time serialization guarantee.
  3. Removing the join keeps REPLAY a single indexed lookup.
  4. A multi-endpoint generalization would re-introduce the separate table; the migration is mechanical.
- **`payload_hash`** is SHA-256 over canonical JSON of *payload-identity fields only* — see §9.
- **`balance_after`** on every ledger entry makes the ledger self-describing: reconstructing point-in-time balance is O(1) per entry, and reconciliation against `wallets.balance` is a trivial join.
- **Direction + unsigned amount** is chosen over signed amount because it lets `UNIQUE(transfer_id, direction)` *structurally* prevent two-DEBITs-same-transfer bugs. With signed amounts, the only witness for the double-entry invariant would be application code.
- **Partial index `ix_transfers_pending`** serves the recovery worker. PROCESSED rows dominate; a partial index keeps the index small and hot in cache.
- **`processed_at`** is set on both terminal states (PROCESSED and FAILED), not just success. Naming retained for clarity in the common case; I10 reflects this.

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

The idempotency key lives in the **`Idempotency-Key` HTTP header**, not the request body. This follows the Stripe convention and keeps the payload identity (from/to/amount) cleanly separable from the request identity (key) — which is the conceptual basis for `payload_hash` (§9).

**Wire format is camelCase** for the body fields. Internal code and database schema use snake_case; the transport layer maps between them.

**Validation, before any DB write:**
- `Idempotency-Key` header present and non-empty, max 128 chars.
- `amount > 0`, integer.
- `fromWalletId != toWalletId`.
- Both wallet IDs are non-empty strings.

Wallet existence is enforced at T1 by the foreign key — if a wallet does not exist, T1's INSERT fails and the handler maps that to 404.

### Response codes

| Status | Condition | Body |
|---|---|---|
| `201 Created` | New transfer terminated (PROCESSED or FAILED) | Transfer object |
| `201 Created` | Idempotent replay — same status as the original, per Stripe convention | The original transfer object |
| `400 Bad Request` | Missing `Idempotency-Key` header | `{ "error": "MISSING_IDEMPOTENCY_KEY" }` |
| `400 Bad Request` | Malformed payload, non-positive amount, same wallet, missing field, unknown field | `{ "error": "VALIDATION", "detail": ... }` |
| `404 Not Found` | Wallet does not exist | `{ "error": "WALLET_NOT_FOUND" }` |
| `409 Conflict` | Same idempotency key reused with a different payload | `{ "error": "IDEMPOTENCY_KEY_REUSE" }` |
| `503 Service Unavailable` | Transient (DB unavailable, lock timeout). No state persisted. | Retry with backoff |

**Design note — FAILED returns 201, not 4xx.** Insufficient funds is a deterministic, replayable terminal outcome — not a malformed request. Reserving 4xx for client errors keeps idempotent replay semantics clean: a FAILED transfer replays identically to a PROCESSED one.

**Replay returns the same HTTP status as the original response**, not a different code. Clients cannot tell first-time-success from retry-success — which is the contract they want.

### Transfer object (response body)

```json
{
  "id":             "01HXYZ...",
  "status":         "PROCESSED",
  "fromWalletId":   "wallet_1",
  "toWalletId":     "wallet_2",
  "amount":         100,
  "failureReason":  null,
  "createdAt":      "2026-05-20T10:00:00Z",
  "processedAt":    "2026-05-20T10:00:00Z"
}
```

### `GET /wallets/{id}`

Returns the current balance and metadata for a wallet.

```json
Response 200:
{
  "id":        "wallet_1",
  "balance":   400,
  "createdAt": "2026-05-20T10:00:00Z",
  "updatedAt": "2026-05-20T10:05:00Z"
}
```

| Status | Condition |
|---|---|
| `200 OK` | Wallet exists |
| `404 Not Found` | No wallet with that ID |
| `400 Bad Request` | Empty ID in path |

Pure read; no locks. Returns committed state at read time. A read concurrent with a transfer in flight returns either the pre-transfer or post-transfer balance, never an intermediate value (§11).

### `GET /wallets/{id}/transfers`

Returns recent transfers involving the wallet, as either source or destination. Most recent first.

Query params:
- `limit` (1–100, default 50): page size.
- `before` (RFC3339Nano timestamp, optional): keyset cursor — return transfers created strictly before this timestamp.

```json
Response 200:
{
  "walletId": "wallet_1",
  "count":    2,
  "transfers": [
    { /* transferResponse */ },
    { /* transferResponse */ }
  ],
  "nextBefore": "2026-05-20T10:00:00Z"   // present only if there may be more
}
```

| Status | Condition |
|---|---|
| `200 OK` | Wallet exists (history may be empty) |
| `404 Not Found` | No wallet with that ID |
| `400 Bad Request` | Invalid `limit` or `before` |

**Design notes**

- Includes transfers in all states (PENDING, PROCESSED, FAILED). FAILED is included intentionally — a client needs to see "your transfer was rejected" not just "your money didn't move."
- Empty history (wallet exists, no transfers) returns `200` with an empty array, distinct from `404`.
- Pagination uses keyset on `(created_at, id)` rather than offset to keep behavior stable under concurrent writes.

---

## 6. Expected Behavior

For any well-formed `POST /transfers` request, the system guarantees:

1. **Exactly one terminal transfer record** exists for the given `idempotencyKey`. The record is `PROCESSED` (money moved) or `FAILED` (business rejection).
2. **Exactly two ledger entries** exist for a `PROCESSED` transfer (DEBIT on source, CREDIT on destination); **zero** for `FAILED`.
3. **`fromWallet.balance` decreases by `amount` and `toWallet.balance` increases by `amount`** iff `PROCESSED`. No observable intermediate state where only one side moved.
4. **Every retry with the same `idempotencyKey` and same payload returns the same response** (status code and body), forever (until the row is archived).
5. **No wallet balance is ever negative.**
6. **No two concurrent requests cause a deadlock.** Concurrent transfers on disjoint wallet sets fully parallelize. Concurrent transfers sharing a wallet serialize on that wallet's row lock.

"Expected" explicitly includes business rejection: an insufficient-funds transfer is *not an error* — it is a correct, deterministic, terminal `FAILED` outcome that replays identically.

---

## 7. Transfer Algorithm

Two transactions. T1 reserves the idempotency key and records intent. T2 moves money atomically. REPLAY is the read-path for duplicates; when REPLAY finds a `PENDING` row it runs T2 itself on that row (same transaction that holds the row lock).

### T1 — Reserve the idempotency key

```
BEGIN
  INSERT INTO transfers (id, idempotency_key, payload_hash,
                         from_wallet_id, to_wallet_id, amount,
                         status = 'PENDING')
  -- On UNIQUE(idempotency_key) violation → key already exists → REPLAY
  -- On FK violation on from/to wallet → 404 (no transfer row created, no idempotency cached)
COMMIT
```

After T1 commits, the key is durably reserved and intent is recorded. No money has moved.

### T2 — Move the money atomically

Everything below commits together or rolls back together. T2 is also entered by REPLAY when an existing transfer is found in `PENDING` — in that case, the SELECT FOR UPDATE on the transfer row was already done by REPLAY in the same transaction, and execution continues from step 2.

```
BEGIN
  -- 1. Lock the transfer row; bail if not PENDING (race / already done)
  SELECT id, status, from_wallet_id, to_wallet_id, amount
    FROM transfers WHERE id = :id FOR UPDATE;
  IF status <> 'PENDING' THEN GOTO REPLAY; END IF;

  -- 2. Lock both wallets in deterministic (ascending) order
  SELECT id, balance FROM wallets
    WHERE id IN (:from, :to) ORDER BY id FOR UPDATE;

  -- 3a. Insufficient funds → FAILED (terminal, no money movement)
  IF from.balance < :amount THEN
    UPDATE transfers
       SET status = 'FAILED',
           failure_reason = 'INSUFFICIENT_FUNDS',
           processed_at = now()
     WHERE id = :id AND status = 'PENDING';
    COMMIT;
    RETURN FAILED;
  END IF;

  -- 3b. Sufficient funds → PROCESSED (terminal, atomic money movement)
  new_from := from.balance - :amount;
  new_to   := to.balance   + :amount;

  INSERT INTO ledger_entries (transfer_id, wallet_id, direction, amount, balance_after) VALUES
    (:id, :from, 'DEBIT',  :amount, new_from),
    (:id, :to,   'CREDIT', :amount, new_to);

  UPDATE wallets SET balance = new_from, updated_at = now() WHERE id = :from;
  UPDATE wallets SET balance = new_to,   updated_at = now() WHERE id = :to;

  UPDATE transfers
     SET status = 'PROCESSED', processed_at = now()
   WHERE id = :id AND status = 'PENDING';
  -- rows_affected must be 1; if 0, rollback and goto REPLAY (anomaly logged)

  COMMIT
  RETURN PROCESSED
```

### REPLAY — pure read of the durable outcome (or in-line execution of stranded PENDING)

Entered on `UNIQUE(idempotency_key)` violation in T1, **or** when T2's status check finds the row already terminal.

```
BEGIN
  existing := SELECT * FROM transfers WHERE idempotency_key = :key FOR UPDATE;

  IF existing.payload_hash <> incoming_payload_hash:
      ROLLBACK; RETURN 409 IDEMPOTENCY_KEY_REUSE       -- different payload, same key

  IF existing.status IN ('PROCESSED', 'FAILED'):
      ROLLBACK; RETURN 201 with existing transfer object   -- pure read; immutable; safe

  IF existing.status = 'PENDING':
      -- Original holder either crashed or another instance was mid-T2.
      -- If another instance is mid-T2, our FOR UPDATE blocked until they committed —
      -- on resumption status is terminal (handled above).
      -- If we reach here, T1 committed and T2 was never run.
      -- We hold the row lock; continue this same transaction as T2 from step 2.
      GOTO T2 step 2
COMMIT
```

The retry handler's transaction *is* T2 when it finds PENDING. There is no second transaction; the SELECT FOR UPDATE lock acquired in REPLAY carries through into the wallet locks and ledger writes. This is what makes "REPLAY runs T2" safe — no lock is dropped between observing PENDING and executing the money movement.

### Why this is structurally correct

1. **Idempotency at the API:** The UNIQUE constraint on `idempotency_key` serializes T1 INSERTs at the database. Two simultaneous identical requests both attempt the INSERT; exactly one commits, the other receives `unique_violation` and falls into REPLAY. No application-level check, no TOCTOU, no race window.

2. **Idempotency at the executor:** `UNIQUE(transfer_id, direction)` on the ledger prevents the legs from being posted twice even if T2 is re-attempted (retry-handler running T2 after a crash, or the recovery worker). T2 is therefore safely re-runnable.

3. **Atomicity:** Both money-movement paths (PROCESSED, FAILED) commit inside a single transaction. There is no observable instant where one wallet moved and the other did not, no instant where the ledger and balances disagree.

4. **State monotonicity:** The conditional `UPDATE ... WHERE status='PENDING'` enforces I7 at the DB level. A buggy code path attempting to flip a terminal transfer affects zero rows; the application treats `rows_affected=0` as "raced, go to REPLAY" and logs the anomaly.

5. **No client poll loop:** Because the retry handler runs T2 in-line when it finds PENDING, the client never receives a "still PENDING, try later" response. The recovery worker is reserved for the genuine long-tail case — a client that crashed and will never retry.

---

## 8. Concurrency & Race Conditions

Four distinct races, four distinct defenses.

**Race 1 — Two identical requests, same `idempotencyKey`, simultaneously.**
Both attempt T1's INSERT. The `UNIQUE` index serializes them at the database. One INSERT commits; the other receives `unique_violation` and falls into REPLAY. The unique index *is* the mutex.

**Race 2 — Two different transfers draining the same wallet.**
The classic lost-update / overdraft pattern. Prevented by `SELECT ... FOR UPDATE` on wallet rows in T2. The second transaction blocks until the first commits, then reads the post-first-transfer balance and re-evaluates the funds check against current state. `CHECK (balance >= 0)` is the final backstop.

**Race 3 — Crossing transfers A→B and B→A.**
Would deadlock if one transaction locked A then B and the other locked B then A. Eliminated by **always locking wallets in ascending ID order** in T2's wallet SELECT. Both transactions request locks in the same global order; one waits cleanly.

**Race 4 — Retry-handler T2 vs. recovery-worker T2 on the same PENDING row.**
Both contend for `SELECT * FROM transfers ... FOR UPDATE`. One acquires the lock and runs T2 to terminal; the other blocks, then on resumption sees a non-PENDING row and goes to read-only REPLAY. `UNIQUE(transfer_id, direction)` is the additional backstop if the status check ever malfunctions. The recovery worker uses `FOR UPDATE SKIP LOCKED` so it skips rows the retry handler is actively working on.

### Choice of strategy

**Pessimistic row-level locks (`SELECT ... FOR UPDATE`) at READ COMMITTED isolation, with deterministic lock order on wallets by ascending `id`.**

Considered and rejected:
- **Optimistic locking (`version` column).** Composability is awkward for multi-wallet operations — you'd version-check two rows and retry the entire transaction on conflict. Worth it only when contention is rare and lock overhead matters; not the case here. Also: `FOR UPDATE` already provides everything optimistic locking would, so stacking them adds a column with no incremental safety.
- **SERIALIZABLE isolation.** Correct, but pushes failure handling to the app via `40001` retries; harder to reason about which invariant is preserved where. READ COMMITTED is sufficient because every contended read in T2 is an explicit `FOR UPDATE`.
- **Advisory locks.** Decouples locking from row identity — possible deadlocks if the key scheme is wrong, and audits get harder.

---

## 9. Idempotency Strategy

**Storage.** `transfers.idempotency_key` with a `UNIQUE` constraint. For a single-endpoint service the key maps to exactly one transfer aggregate and inherits its lifecycle. The brief suggests a separate `idempotency_records` table; we merged it because for one endpoint the separate table would carry only `(key, transfer_id, payload_hash)` — fields that fit on `transfers` itself with no semantic loss. A multi-endpoint generalization would re-introduce the table; the change is mechanical.

**Duplicate detection.** At write time, by the UNIQUE-constraint violation on T1's INSERT — never by a pre-SELECT; (relying on the constraint does not).

**Original-result return.** The terminal transfer row *is* the stored result. Because PROCESSED/FAILED are immutable, REPLAY reconstructs the exact original response by reading that row.

**Side-effect prevention — two independent structural layers:**

1. `UNIQUE(idempotency_key)` ensures exactly one request ever proceeds past T1 to execute T2. All others can only *read*.
2. `UNIQUE(transfer_id, direction)` ensures even an internal re-execution (retry-handler-runs-T2, recovery worker) cannot post a transfer's legs twice.

The financial side effect is therefore structurally reachable at most once.

**Key reuse with a different payload.** `payload_hash` is SHA-256 over canonical JSON of the **payload-identity fields only** — `fromWalletId`, `toWalletId`, `amount`. The `idempotencyKey` itself is the *index* into the cache, not part of the identity check. Canonicalization rules:

- Keys sorted lexicographically.
- No whitespace.
- Integers serialized as digit strings (no decimal point, no exponent).
- Absent fields are omitted (no `null`).
- UTF-8 encoding.

Concrete example: a request with payload `{fromWalletId: "wallet_1", toWalletId: "wallet_2", amount: 100}` hashes the canonical bytes of `{"amount":"100","fromWalletId":"wallet_1","toWalletId":"wallet_2"}`.

A mismatch in REPLAY returns 409, never silently honors the request.

**Key lifetime.** Keys are retained for the lifetime of the transfer row. A production deployment archives transfers older than N days; the UNIQUE constraint on live rows continues to block reuse for as long as the row exists. Archival is out of scope for this submission.

**What is NOT cached.** Pre-T1 validation errors (400 from malformed input, 404 from non-existent wallet) are not cached because no transfer row is created. A retry of such a request re-validates against current state. This is a deliberate trade-off: validation outcomes are not stable across changes to the world (e.g., a wallet is created later), so caching them would be wrong.

### 9.1 Why we don't have separate `idempotency_records` or `transfer_events` tables

The brief suggests `idempotency_records` and an audit-style `transfer_events` table. We deliberately have neither, and they're absent for the same underlying reason: **the data they would carry already lives somewhere in this schema, more cheaply and with stronger structural guarantees.**

**Why no `idempotency_records` table.** A separate table would carry `(key PRIMARY KEY, transfer_id, payload_hash, response_status, response_body, created_at)`. In a single-endpoint service:

- The `(key, transfer_id, payload_hash)` portion is a 1-to-1 with the transfer aggregate — we put those fields on `transfers` itself (`idempotency_key UNIQUE`, `payload_hash`).
- The `(response_status, response_body)` portion is unnecessary: the response is **deterministic from the transfer's terminal state**. `status=PROCESSED` reconstructs the success response; `status=FAILED` reconstructs the FAILED response with `failure_reason`. Caching the response body would store information we can already derive.
- The `UNIQUE` constraint on `transfers.idempotency_key` provides exactly the same write-time serialization guarantee that a `PRIMARY KEY` on a separate table would.

The trade-off: in a multi-endpoint API where idempotency keys span resources, the separate table becomes correct because the response is no longer derivable from a single aggregate's state. The migration is mechanical and would happen the moment we add a second endpoint that needs idempotency. For one endpoint, the merged design is structurally equivalent and saves a join.

**Why no `transfer_events` audit table.** A typical audit table records every state transition: `(transfer_id, from_status, to_status, reason, actor, changed_at)`. In our design, the audit information is captured by two existing facts:

- **The state machine is monotonic and terminal states are immutable** (I7). A transfer's history therefore reduces to its current state + `processed_at` + `failure_reason`. There is no "what was its previous status?" question to answer because the only legal predecessor of a terminal state is `PENDING`.
- **The ledger is append-only and immutable** (§12). For PROCESSED transfers, the two ledger entries record the exact financial movement with `balance_after` snapshots — this is the audit trail for the actual money-moving event. For FAILED transfers, there are no ledger entries by design (I6); the `transfer` row's `failure_reason` and `processed_at` carry the audit.

What this gives up vs. a dedicated audit table:

- *Multiple transitions per transfer.* We have at most one transition per transfer (PENDING → terminal). A more complex state machine would benefit from an audit table; ours doesn't.
- *Operator-initiated transitions.* If a human ops user could force a state change (e.g., manually marking a stuck transfer as FAILED), audit fields like `actor` would matter. We have no such administrative interface.
- *Queryable change history.* `transfer_events` would let you ask "show me all transitions in the last hour." We instead answer this via `SELECT * FROM transfers WHERE processed_at >= now() - interval '1 hour'`, which is sufficient because we only have one transition per transfer.

What we keep: **structural enforcement of the audit invariants the table would protect.** Specifically:

| Audit-table invariant | How we enforce it without the table |
|---|---|
| Terminal states are immutable | `UPDATE transfers SET status = ... WHERE id = ? AND status = 'PENDING'` — rows_affected=0 if violated; logged as anomaly |
| No state transition is silently lost | T2 commits state transition + balance updates + ledger inserts atomically. Either all are observed or none are |
| Reversals are visible | The ledger's append-only nature means a reversal is a *new* compensating entry pair, never an UPDATE — the original entries remain |
| Money movement is fully auditable | `ledger_entries(transfer_id, wallet_id, direction, amount, balance_after, created_at)` is the audit trail for money. `SELECT * FROM ledger_entries WHERE wallet_id = ? ORDER BY id` reconstructs a wallet's full financial history |

When this trade-off would flip: as soon as the state machine grows (multi-step async settlement, hold/release, reversals as first-class transitions, multi-actor authorization), `transfer_events` becomes correct because the audit signal stops being derivable from current state alone. It's a deliberate scope decision for the assignment, not an oversight.

---

## 10. Retry & Failure Model

"Safe" means: no money created, no money destroyed, and the client can always reach a correct terminal state by retrying with the same key.

| # | Failure point | What persisted | Outcome on retry (same key) |
|---|---|---|---|
| 1 | Network drops before request reaches server | nothing | First-time execution |
| 2 | Crash after T1 commit, before T2 starts | transfer = PENDING | Retry handler acquires row lock, sees PENDING, runs T2 itself |
| 3 | Crash during T2 (before its COMMIT) | T2 rolled back atomically | Transfer still PENDING → retry handler runs T2 |
| 4 | Crash after T2 commit, before HTTP response | transfer = terminal | Retry → REPLAY → original response |
| 5 | Two identical requests race | one wins T1 INSERT | Loser → REPLAY → winner's outcome |
| 6 | Concurrent drains on one wallet | row lock serializes | No overdraft, no lost update |
| 7 | Crossing transfers A→B / B→A | deterministic lock order | One waits, no deadlock |
| 8 | Insufficient funds | transfer = FAILED (terminal) | Retry → same FAILED, idempotent |
| 9 | Key reused with different payload | original transfer untouched | 409, no side effect |
| 10 | DB unavailable mid-request | nothing committed | 503 → retry with backoff |
| 11 | Client crashes and never retries | transfer = PENDING (stranded) | Recovery worker runs T2 (see §13) |
| 12 | Pre-T1 validation failure (400/404) | nothing persisted | Re-validates against current state; outcome may differ if world changed |

**Structural reason every row is safe.** The only step that moves money is a single atomic transaction (T2), gated by an idempotency key whose terminal outcome is immutable and only ever *read* on replay. A crash either happens before T2 (nothing moved, retry runs it) or after it (it fully completed, retry just reads it). There is no halfway because the database does not permit one.

### Client retry contract

- Always retry with the **same** idempotency key. A retry without the original key is a *new transfer* — this is a client correctness requirement.
- Retry only on retryable signals: network timeout, 503, connection reset. Never auto-retry 400/409 (deterministic client errors).
- Exponential backoff with jitter (e.g. 200 ms base, factor 2, full jitter, 5 s cap, bounded attempts).

---

## 11. Consistency Expectations

- **Within a transfer:** ACID. All five side effects (transfer row terminal, two ledger entries, two balance updates) commit together or not at all.
- **Across transfers on the same wallet:** serialized via row locks. Effective ordering is commit order.
- **Across transfers on disjoint wallet sets:** fully concurrent.
- **Reads of balance:** see committed state; do not block on in-flight transfers. A `SELECT balance FROM wallets WHERE id=?` mid-transfer returns either the pre-transfer or post-transfer balance, never an intermediate value, never a negative value.
- **Ledger as source of truth:** at any committed snapshot, `wallets.balance == latest ledger_entries.balance_after` for every wallet. A periodic reconciliation job asserts this and alerts on drift, even though by construction it should never happen.
- **State machine:** terminal states (`PROCESSED`, `FAILED`) are immutable. A transfer never moves out of a terminal state. A transfer in `PENDING` will eventually reach a terminal state via the retry handler or recovery worker.

---

## 12. Side Effects

| Side effect | When | Mutability |
|---|---|---|
| `transfers` row inserted (PENDING) | T1 commit | Intent record; key permanently reserved |
| `wallets.balance` mutated (both rows) | T2 commit, atomically | Mutable on each transfer; reversal requires a new compensating transfer (ledger remains the audit trail) |
| Two `ledger_entries` inserted | T2 commit, atomically | **Immutable.** Corrections are new compensating entries, never UPDATE/DELETE |
| `transfers.status` → terminal | T2 commit | **Immutable.** Terminal states never change |

No external side effects (notifications, webhooks) on the hot path. If added later, they must be emitted from a transactional outbox written inside T2, never a best-effort post-commit call.

---

## 13. Recovery Worker

A background worker for the long-tail case: client crashed and will never retry.

```sql
SELECT id FROM transfers
WHERE status = 'PENDING'
  AND created_at < now() - INTERVAL :threshold
ORDER BY created_at
LIMIT 100
FOR UPDATE SKIP LOCKED;
```

For each row, run T2. Re-running is safe because:
- T2's first step (`SELECT ... FOR UPDATE` on the transfer) re-checks status under lock.
- `UNIQUE(transfer_id, direction)` makes a duplicate ledger post impossible at the DB level.
- `FOR UPDATE SKIP LOCKED` lets multiple worker instances run without contention and skips rows the retry handler is actively working.

**`RECOVERY_PENDING_THRESHOLD_SECONDS`** is configurable. Default: 30s (≈ 5× expected p99 T2 latency for assignment-scale). Lower = faster recovery, more contention with in-flight retries; higher = slower recovery, less contention. The retry handler running T2 in-line means this threshold can be conservative — the worker is a safety net, not the hot path.

---

## 14. Architecture

Four layers, matching the brief's terminology. Strict dependency direction: outer layers depend inward; the domain depends on nothing.

```
┌──────────────────────────────────────────────────────────────────┐
│ Handler layer (HTTP)                                              │
│   • Parse + validate JSON, map domain errors to status codes      │
│   • Translate between camelCase wire format and snake_case domain │
│   • Zero business logic                                           │
├──────────────────────────────────────────────────────────────────┤
│ Service layer                                                     │
│   • TransferService.execute(cmd)                                  │
│   • Owns transaction boundaries (T1, T2, REPLAY)                  │
│   • Orchestrates the algorithm in §7                              │
│   • Idempotency policy lives here                                 │
├──────────────────────────────────────────────────────────────────┤
│ Domain layer (pure, no I/O, fully unit-testable)                  │
│   • Wallet: debit() / credit() enforcing balance >= 0             │
│   • Transfer: state machine — only PENDING → {PROCESSED, FAILED}  │
│   • LedgerEntry: invariant that a transfer's legs sum to zero     │
├──────────────────────────────────────────────────────────────────┤
│ Repository layer (ports & adapters)                               │
│   • WalletRepository, TransferRepository, LedgerRepository        │
│   • UnitOfWork / TransactionManager (BEGIN, COMMIT, FOR UPDATE)   │
│   • Implements interfaces declared by the service layer           │
└──────────────────────────────────────────────────────────────────┘
```

### Key boundaries

- **Transaction boundaries are owned by the service layer**, not the repositories. One use-case invocation = one well-defined set of DB transactions. Repositories never `COMMIT` on their own.
- **State machine lives in the `Transfer` domain entity.** `transfer.mark_processed()` throws if not PENDING. Illegal transitions are impossible to express, not merely discouraged. SQL UPDATEs to `status` go through the entity, never directly.
- **Domain is pure.** No SQL, no clock, no framework. Unit-testable exhaustively — every state transition, the zero-sum ledger invariant, the non-negative balance rule — with zero infrastructure. This is exactly the code path that must never have a bug.
- **Dependency inversion:** the service layer declares `WalletRepository` etc. as interfaces; the repository layer implements them. Swapping Postgres for another store touches only the outermost layer.

---

## 15. Observability Expectations

The brief lists observability as a documentation-first concern. The following is *designed* but not built in this submission's scope (per §17). The metrics, logs, and alerts production would need:

**Metrics**
- Transfer outcome rate by status (PROCESSED / FAILED / 409 / 503), tagged by failure_reason where applicable.
- T1 latency, T2 latency, REPLAY latency (p50/p95/p99).
- Lock wait time on wallet rows.
- Recovery worker queue depth (count of PENDING transfers older than threshold).
- Reconciliation drift (count of wallets where `wallets.balance != latest ledger balance_after`).

**Logs (structured)**
- Every 409 (idempotency key reuse) with both payload hashes.
- Every `rows_affected=0` from the conditional UPDATE on transfers — indicates a state-machine anomaly.
- Every deadlock or serialization failure (should be ~0).
- Every recovery worker run with the transfers it picked up.

**Alerts**
- Reconciliation drift > 0 — page immediately. Money discrepancies are paging incidents, never auto-corrected.
- PENDING transfers older than 5 × threshold — recovery worker not keeping up or stalled.
- 503 rate above baseline — DB pressure or pool exhaustion.
- Sudden FAILED rate spike — possible upstream issue (clients sending bad data) or downstream issue (data inconsistency).

---

## 16. Testing Strategy

Tests are structured by invariant, not by file. Every concurrency test ends with an `assert_invariants()` helper that re-checks I1, I3, I4, I6 against the database. If an invariant ever fails, the test fails — regardless of what the test was specifically asserting.

### Unit tests (domain layer, no DB)
- Wallet: `debit` rejects when amount exceeds balance; `credit` accepts; both update `balance` correctly.
- Transfer: state transitions — PENDING→PROCESSED OK, PENDING→FAILED OK, PROCESSED→anything throws, FAILED→anything throws, PENDING→PENDING throws.
- LedgerEntry pair: rejects construction if DEBIT + CREDIT amounts don't match or wallets are the same.

### Integration tests (with real Postgres)
- **Happy path:** `transfer(W1, W2, 100)` → both balances updated, two ledger entries, transfer PROCESSED. `assert_invariants()`.
- **Insufficient funds:** balance unchanged, no ledger entries, transfer FAILED with reason. `assert_invariants()`.
- **Idempotency replay — same payload:** two sequential calls with same key → identical response, one transfer in DB, one ledger pair.
- **Idempotency replay — different payload:** second call returns 409, original transfer unchanged.
- **Non-existent wallet:** 404, no transfer row, no idempotency cached.
- **Self-transfer:** 400 at validation, no transfer row.

### Concurrency tests (real Postgres, parallel goroutines/threads)
- **I4 — overdraft prevention:** wallet starts at 500. Ten parallel transfers of 100 from it. Exactly 5 succeed (PROCESSED), exactly 5 fail (FAILED with INSUFFICIENT_FUNDS), final balance = 0. `assert_invariants()`.
- **I8 — idempotency race:** 50 parallel calls with the same key + same body. Exactly one transfer in DB, exactly one ledger pair, all 50 responses identical (byte-equal).
- **Race 3 — crossing transfers:** N pairs of A→B and B→A in parallel. No deadlocks; `assert_invariants()` holds.
- **Crash + recovery:** simulate crash mid-T2 (rollback before COMMIT); verify transfer is PENDING; verify retry-handler-run-T2 drives it to terminal; `assert_invariants()`.

### TDD discipline
Per the brief: write the failing test first (red), implement minimally (blue), refactor while keeping tests green. The I4 and I8 concurrency tests are the highest-leverage tests — write them before any business logic.

---

## 17. Out of Scope / Future Work

Explicitly *not* built, despite being good engineering, to honor the brief's "focus on correctness and clarity, not feature completeness":

- **Wallet creation endpoint.** Wallets are pre-seeded via database fixtures for tests. No public endpoint.
- **`GET /transfers/{id}`** (Optional Enhancement, not implemented). `GET /wallets/{id}` and `GET /wallets/{id}/transfers` are implemented — see §5.
- **Multi-currency.** Single currency; adding it means a column on `wallets` and a currency-match check in T2.
- **Transactional outbox for webhooks/notifications.** Would live inside T2; not needed because there are no external consumers.
- **Reconciliation checkpoint table.** Production would snapshot `(wallet_id, ledger_entry_id, balance)` so reconciliation is O(delta). Out of scope; this design's reconciliation is O(history) and run rarely.
- **Audit table `transfer_state_changes`.** Production hygiene that provides queryable transition history. See §9.1 for the consolidated rationale on why our design doesn't have this OR a separate `idempotency_records` table.
- **Metrics emission and alerting infrastructure.** Designed in §15. Structured logging IS built (`log/slog` via `internal/logger`, JSON output, request-scoped via context); metrics and alerts are not.
- **Rate limiting, authentication, request signing.** Out of scope.
- **Archival of old transfers / idempotency keys.** Out of scope; for this submission keys live as long as their transfer rows.

---

## 18. Mapping to Assignment Evaluation Criteria

| Axis | Where addressed |
|---|---|
| **Database Design** — schema clarity, constraints, integrity, indexes | §4 (schema), §3 (every invariant maps to a structural enforcement) |
| **Transaction Strategy** — boundaries, locks, race handling | §7 (T1/T2/REPLAY), §8 (four races, four defenses) |
| **Idempotency Strategy** — correctness under retries, dedup | §9 (structural via UNIQUE), §10 (failure model row-by-row) |
| **Code Structure** — clean layering, separation of concerns | §14 (four layers, owned transaction boundaries, pure domain) |
| **Testing** — meaningful coverage, correctness validation | §16 (invariant-driven, concurrency tests, TDD discipline) |

### The four functional guarantees from the brief

| Guarantee | Mechanism |
|---|---|
| Idempotent request handling | `UNIQUE(idempotency_key)` + `payload_hash` + REPLAY read path |
| Double-entry ledger recording | `ledger_entries` with `direction` + `UNIQUE(transfer_id, direction)` + atomic T2 |
| Correct balance tracking | Stored `wallets.balance` + ledger source of truth + I3 reconciliation + `CHECK >= 0` |
| Safe concurrent execution | Sorted `FOR UPDATE` on wallets; row lock on transfer; READ COMMITTED |
| Exactly-once at the API | T1's UNIQUE constraint (one execution per key) + T2's UNIQUE(transfer_id, direction) (one ledger pair per transfer) |

---

**Implementation order, per §16's TDD note:** write the I4 (overdraft) and I8 (idempotency race) concurrency tests first; scaffold migrations from §4 verbatim; implement T1, T2, REPLAY as a direct translation of §7; then let the remaining tests drive the rest.
