# System Invariants

This document is the canonical list of properties that **must always hold** for the wallet transfer service to be correct. Every design decision in [`DESIGN.md`](./DESIGN.md) exists to make at least one of these invariants true by construction.

Each invariant is stated three ways:

1. **What it says** — the property in one sentence.
2. **How it's enforced at write time** — the structural mechanism (DB constraint, transaction shape, code structure) that makes violation impossible or extremely difficult.
3. **How it's verified at runtime** — the test, query, or invariant check that would catch a regression.

The list is numbered I1–I10 throughout the codebase. If you see a reference like "I4 in `assertInvariants`," it means "the I4 row in this table."

---

## Quick map

| # | Invariant | Primary enforcement |
|---|---|---|
| I1 | Per transfer, ledger entries sum to zero | `UNIQUE(transfer_id, direction)` + atomic T2 |
| I2 | Globally, system-wide debits = credits | I1 + atomic T2 |
| I3 | `wallets.balance` ≡ latest `ledger_entries.balance_after` | Balance + ledger updated in the same T2 commit |
| I4 | `wallets.balance >= 0` always | App-layer check under `FOR UPDATE` + DB `CHECK` |
| I5 | `ledger_entries.balance_after >= 0` | Computed under lock + DB `CHECK` |
| I6 | Each transfer has 0 (FAILED) or 2 (PROCESSED) ledger entries | Atomic T2: writes both legs or none |
| I7 | State monotonicity: `PENDING → {PROCESSED, FAILED}`, terminals immutable | Domain entity + conditional `UPDATE ... WHERE status='PENDING'` |
| I8 | At most one transfer per `idempotency_key` | `UNIQUE(idempotency_key)` |
| I9 | Same key + different payload is rejected | `payload_hash` comparison in REPLAY |
| I10 | `processed_at IS NOT NULL ⇔ status ∈ {PROCESSED, FAILED}` | Set in the same UPDATE that transitions to terminal |

---

## I1 — Per transfer, ledger entries sum to zero

**The property.** For any `transfer_id` that has ledger entries, the sum of CREDIT amounts equals the sum of DEBIT amounts. Money is neither created nor destroyed by the act of recording a transfer.

**Write-time enforcement.**
- `UNIQUE(transfer_id, direction)` on `ledger_entries` ensures at most one DEBIT and at most one CREDIT per transfer.
- The service writes both legs in a single `INSERT ... VALUES (...), (...)` inside T2, using the same `amount` variable for both. They commit atomically or not at all.
- The domain layer's `LedgerEntry` constructors (`NewDebitEntry`, `NewCreditEntry`) take an `amount` parameter — there's no path to constructing them with mismatched amounts.

**Runtime verification.**
- `assertInvariants()` in `tests/helpers_test.go` runs a SQL query asserting that no PROCESSED transfer has unbalanced legs:
  ```sql
  SELECT t.id, SUM(CASE WHEN direction='DEBIT' THEN amount END) AS debits,
                SUM(CASE WHEN direction='CREDIT' THEN amount END) AS credits
  FROM transfers t JOIN ledger_entries l ON l.transfer_id = t.id
  WHERE t.status = 'PROCESSED'
  GROUP BY t.id
  HAVING SUM(CASE WHEN direction='DEBIT' THEN amount END)
      <> SUM(CASE WHEN direction='CREDIT' THEN amount END)
  ```
- Every concurrency test ends with `assertInvariants()`. Any divergence fails the test.

**Regression that would catch a removal of this invariant.** `TestRegression_LedgerUniqueDirection_PreventsDoubleDebit` directly attempts a second DEBIT for the same transfer and asserts a unique-violation error. Drop the constraint, this test fails immediately.

---

## I2 — Globally, system-wide debits = credits

**The property.** Across the entire system, the sum of all DEBIT entries equals the sum of all CREDIT entries. The system is closed: no money is created or destroyed, ever.

**Write-time enforcement.** Follows from I1 by induction over committed transfers: if every transfer's legs balance, the global sum balances. No additional mechanism needed beyond I1 + atomic T2.

**Runtime verification.** A reconciliation query (not currently scheduled in production but trivial to add):
```sql
SELECT SUM(CASE WHEN direction='DEBIT' THEN amount ELSE 0 END) AS total_debits,
       SUM(CASE WHEN direction='CREDIT' THEN amount ELSE 0 END) AS total_credits
FROM ledger_entries;
-- total_debits must equal total_credits
```
Drift here is a paging incident, not an auto-correction event.

**Regression coverage.** Implicit — any I1 failure would also be an I2 failure, and the I1 tests are the active line of defense.

---

## I3 — `wallets.balance` equals the ledger-derived balance for each wallet

**The property.** For every wallet, `wallets.balance` equals the `balance_after` of that wallet's most recent ledger entry (or its seeded balance if it has no entries). The stored balance is a fast projection of the ledger; the ledger is the source of truth.

**Write-time enforcement.** In T2, the application:
1. Computes `new_balance = current_balance ± amount` while holding the wallet's `FOR UPDATE` lock.
2. Writes the ledger entry with `balance_after = new_balance`.
3. Updates `wallets.balance` to `new_balance`.

All three happen in a single transaction. Either all commit together or all roll back. There is no committed snapshot where balance and the latest `balance_after` disagree.

**Runtime verification.**
- `assertInvariants()` runs a query joining `wallets` to the latest `ledger_entries` row per wallet (using `DISTINCT ON (wallet_id) ... ORDER BY wallet_id, id DESC`) and asserts the values match.
- This is the strongest check we have on the ledger-as-source-of-truth claim. Every concurrency test runs it.

**Regression that would catch this.** Any test where money is moved checks I3 transitively. The targeted scenario is `TestConcurrency_NoOverdraftUnderContention` — 10 parallel transfers from one wallet, then `assertInvariants()` confirms wallet balance still matches the ledger.

---

## I4 — `wallets.balance >= 0` always

**The property.** No wallet ever has a negative balance, at any committed snapshot, under any concurrent load.

**Write-time enforcement — two layers:**

1. **Application layer:** `Wallet.Debit(amount)` in `internal/domain/wallet.go` checks `Balance < amount` and returns `ErrInsufficientFunds` if so. The service catches this in T2 and writes a FAILED transfer with no balance change. This check happens **while holding the wallet's `FOR UPDATE` lock**, so no concurrent transfer can change the balance between the check and the write.

2. **Database layer:** `CHECK (balance >= 0)` constraint on `wallets.balance`. If any code path bypasses the application check, the constraint catches it. The transaction aborts; no negative balance is ever committed.

The two layers are independent. A bug in the application check (e.g., wrong comparison operator) leaves the DB constraint as a backstop. A bug that drops the DB constraint leaves the application check as the active defense.

**Runtime verification.**
- `assertInvariants()` includes `SELECT COUNT(*) FROM wallets WHERE balance < 0`. Must be 0.
- The DB constraint itself makes I4 impossible to commit. If a test attempted to write a negative balance, Postgres would error out.

**Regression that would catch this.** `TestConcurrency_NoOverdraftUnderContention` is the load-bearing test: 10 parallel transfers of 100 each from a wallet starting at 500, asserting exactly 5 succeed and 5 fail with INSUFFICIENT_FUNDS, final balance 0. Without the wallet lock OR without the application check, multiple goroutines would see balance=500 simultaneously and over-debit; the test would fail on outcome counts AND on the final balance check.

---

## I5 — `ledger_entries.balance_after >= 0`

**The property.** Every ledger entry's `balance_after` snapshot is non-negative. This is the ledger-side analog of I4 — the audit trail itself can't record a negative balance.

**Write-time enforcement.**
- The `balance_after` value is computed in T2 from the locked balance plus or minus the amount. Since I4 ensures the post-update balance is non-negative, I5 holds by construction.
- `CHECK (balance_after >= 0)` on `ledger_entries.balance_after` as the DB-level backstop.

**Runtime verification.** Implicit via I4 + the DB CHECK. No separate query needed — if I4 holds, I5 holds by construction.

**Regression coverage.** Same tests as I4. If the application bug allowed an over-debit, the DB constraint on either `wallets.balance` OR `ledger_entries.balance_after` would catch it (both fire on the same condition).

---

## I6 — Each transfer has 0 (FAILED) or 2 (PROCESSED) ledger entries

**The property.** A transfer is in one of three states with respect to ledger entries:
- `PENDING`: 0 ledger entries (T2 hasn't run yet).
- `PROCESSED`: exactly 2 ledger entries (one DEBIT, one CREDIT).
- `FAILED`: 0 ledger entries (T2 ran but the funds check rejected; no money movement happens).

There is no committed state in which a PROCESSED transfer has 0 or 1 entries, or in which a FAILED transfer has any entries.

**Write-time enforcement.**
- T2 is a single transaction. The FAILED branch executes `UPDATE transfers SET status='FAILED' ...` and commits, never reaching the ledger insert. The PROCESSED branch executes the ledger insert AND the balance updates AND the status transition, then commits. Either branch commits atomically.
- `UNIQUE(transfer_id, direction)` from I1 prevents 3+ entries per transfer.

**Runtime verification.** `assertInvariants()` runs:
```sql
SELECT COUNT(*) FROM (
  SELECT t.id, t.status, COUNT(l.id) AS leg_count
  FROM transfers t LEFT JOIN ledger_entries l ON l.transfer_id = t.id
  GROUP BY t.id, t.status
) x
WHERE (status = 'PROCESSED' AND leg_count <> 2)
   OR (status = 'FAILED'    AND leg_count <> 0)
   OR (status = 'PENDING'   AND leg_count <> 0)
-- Must return 0
```

**Regression that would catch this.** `TestIntegration_InsufficientFunds` asserts 0 ledger entries after a FAILED. `TestIntegration_HappyPath` asserts 2 entries after PROCESSED. `TestConcurrency_NoOverdraftUnderContention` mixes both and runs `assertInvariants()` over the lot.

---

## I7 — State monotonicity: `PENDING → {PROCESSED, FAILED}`, terminal states immutable

**The property.** A transfer's status follows a one-way state machine. `PENDING` may transition to `PROCESSED` or `FAILED`. Neither terminal state may transition to anything else, including back to PENDING. A `PROCESSED` transfer remains `PROCESSED` forever. A `FAILED` transfer remains `FAILED` forever.

**Write-time enforcement — two layers:**

1. **Domain layer (primary defense):** `Transfer.MarkProcessed(now)` and `Transfer.MarkFailed(reason, now)` both check `t.Status != StatusPending` and return `ErrIllegalStateTransition` if the source state is not PENDING. Illegal transitions cannot be expressed via the domain entity.

2. **Database layer (backstop):** The repository's `UpdateTerminalConditional` method issues:
   ```sql
   UPDATE transfers SET status = ?, ... WHERE id = ? AND status = 'PENDING'
   ```
   The `WHERE status='PENDING'` clause makes the UPDATE a no-op if the transfer has already terminalized. The application checks `rows_affected` — if it's 0, the service treats it as a state anomaly, logs it, and routes the request to REPLAY (which will read and return the existing terminal record).

These two layers protect against different attack surfaces. The domain entity catches in-process bugs (a code path that tries to call `MarkProcessed` twice). The conditional UPDATE catches inter-process races (two T2 attempts on the same row, one losing the race).

**Runtime verification.**
- The conditional UPDATE itself logs an anomaly when `rows_affected = 0`. In production this would be alerted on.
- Indirect: every concurrency test relies on I7 holding, because if it didn't, the test would observe inconsistent state (e.g., the recovery worker overwriting a PROCESSED transfer).

**Regression that would catch this.**
- `TestRegression_I7_TerminalUpdateOnAlreadyProcessed_IsNoOp` — directly drives a transfer to PROCESSED, then bypasses the domain entity to call the repository's `UpdateTerminalConditional` with a fake FAILED transition. Asserts `rows_affected = 0` and that the row is unchanged on a subsequent read.
- `TestRegression_I7_TerminalUpdateOnAlreadyFailed_IsNoOp` — symmetric test from the FAILED side.

If the `WHERE status='PENDING'` clause is removed, both tests would fail (the UPDATE would affect 1 row instead of 0).

---

## I8 — At most one transfer exists per `idempotency_key`

**The property.** The `transfers` table never contains two rows with the same `idempotency_key`. This is the structural foundation for idempotent request handling.

**Write-time enforcement.** `UNIQUE(idempotency_key)` constraint on `transfers`. Postgres serializes concurrent INSERT attempts at the unique index — exactly one commits, all others fail with `SQLSTATE 23505`.

This is intentionally **constraint-based, not check-based**. A pre-`SELECT WHERE idempotency_key = ?` followed by an INSERT would have a TOCTOU race window. The constraint approach has no such window: the uniqueness check and the row insert are a single atomic operation in Postgres.

**Runtime verification.**
- The constraint makes violations impossible to commit.
- `TestConcurrency_IdempotencyRace_SameKey` fires 50 parallel callers with the same key and asserts `countTransfers() == 1`.

**Regression that would catch this.**
- `TestRegression_TransferIdempotencyKeyUnique_PreventsDuplicateRows` — directly attempts to insert a second transfer row with the same key (different transfer ID, different payload) via the repository. Asserts the second INSERT fails with a unique-violation.
- If the constraint is dropped, both this test AND `TestConcurrency_IdempotencyRace_SameKey` would fail.

---

## I9 — Same idempotency key + different payload is rejected, never silently honored

**The property.** If a client reuses an idempotency key with a different `(fromWalletId, toWalletId, amount)` tuple, the system returns 409 IDEMPOTENCY_KEY_REUSE rather than returning the original transfer's result. This catches a specific class of client bug (key-generator collision, accidental reuse, copy-paste).

**Write-time enforcement.**
- The service computes `payload_hash = SHA256(canonical_json(from, to, amount))` before T1.
- T1's INSERT stores the hash on the `transfers` row.
- On REPLAY (entered when T1 hits a unique-violation), the service compares the incoming payload's hash to the stored hash. Mismatch → `ErrIdempotencyKeyReuse` → 409.
- Critically, the `idempotency_key` itself is NOT part of the hash. The hash function `CanonicalPayloadHash(from, to, amount)` doesn't take a key parameter — it's structurally impossible to accidentally include it.

**Runtime verification.**
- `TestCanonicalPayloadHash_Deterministic` — same payload → same hash.
- `TestCanonicalPayloadHash_DifferentAmount`, `_DifferentWallets`, `_OrderMattersForFromTo` — different payloads → different hashes.
- `TestIntegration_IdempotencyReplay_DifferentPayload` — full end-to-end: first call succeeds, second call with same key + different destination returns `ErrIdempotencyKeyReuse`.
- `TestHTTP_ReplaySameKey_DifferentBody_Returns409` — same scenario at the HTTP layer, asserts 409 status code.

**Regression that would catch this.** Any change to the hash function that makes it accidentally include the key, or that makes it non-deterministic, would fail the hash unit tests. Any change to the REPLAY logic that skips the hash comparison would fail the integration tests above.

---

## I10 — `processed_at IS NOT NULL ⇔ status ∈ {PROCESSED, FAILED}`

**The property.** The `processed_at` timestamp is set if and only if the transfer is in a terminal state. A PENDING transfer has `processed_at = NULL`; both PROCESSED and FAILED transfers have it set.

**Write-time enforcement.** The only code path that transitions `status` to a terminal value is `UpdateTerminalConditional`, which sets both `status` and `processed_at` in the same UPDATE statement. They are written atomically; reading them at any committed snapshot shows them consistent.

The domain entity's `MarkProcessed(now)` and `MarkFailed(reason, now)` both set `t.ProcessedAt = &now`, so the in-memory representation is also consistent.

**Runtime verification.** Could be added to `assertInvariants()` as:
```sql
SELECT COUNT(*) FROM transfers
WHERE (status = 'PENDING' AND processed_at IS NOT NULL)
   OR (status IN ('PROCESSED', 'FAILED') AND processed_at IS NULL);
-- Must return 0
```

**Regression coverage.** `TestIntegration_HappyPath` asserts `processed_at` is set on success. `TestIntegration_InsufficientFunds` asserts it's set on FAILED. The shape of `transferResponse` and the domain entity make a PENDING transfer with `processed_at` set impossible to construct via the normal code path.

---

## How invariants combine into exactly-once semantics

The combination that gives the API its exactly-once-per-key property:

- **I8** ensures one row per key (at most one execution attempted per key).
- **I7** ensures the row's terminal status is reached at most once.
- **I1 + I6** ensure the ledger pair is written at most once per row.
- **I9** ensures duplicate requests with the same key but different intent are rejected, not silently honored.

Take any one of these away and the API is no longer exactly-once. They're not redundant; each protects a different attack surface. The regression tests cover each independently because a real-world regression could remove any one of them.

---

## Where this is tested

| Test file | Invariants covered |
|---|---|
| `internal/domain/wallet_test.go` | I4 (domain layer) |
| `internal/domain/transfer_test.go` | I7 (domain layer) |
| `internal/service/hash_test.go` | I9 (hash determinism + scope) |
| `tests/integration_test.go` | I1, I3, I4, I6, I7, I8, I9, I10 (end-to-end, single-threaded) |
| `tests/concurrency_test.go` | I3, I4, I6, I8 (under contention) + `assertInvariants()` runs on every test |
| `tests/regression_test.go` | I7 (direct), I8 (direct), ledger UNIQUE (direct) — defense-in-depth |
| `tests/http_test.go` | I8, I9 (at the HTTP layer with header-based idempotency key) |
| `tests/read_endpoints_test.go` | I3 reflected in `GET /wallets/{id}` returning correct balance |

The `assertInvariants()` helper in `tests/helpers_test.go` checks I1, I3, I4, I6 against the live DB state and is called at the end of every concurrency test. If a concurrency test ever commits a corrupt state, the invariant check fails the test regardless of what the test itself was asserting.
