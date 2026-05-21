# Wallet Transfer Service

A reliable, idempotent, double-entry wallet-to-wallet transfer service built in Go + PostgreSQL.

Full design document lives in [`SYSTEM_DESIGN_DOC.md`](./SYSTEM_DESIGN_DOC.md). This README is for getting it running and oriented in the code.

---

## Quick start

**Requirements:** Go 1.22+, Docker, `make`.

```bash
# 1. Start Postgres (also applies the schema via docker-entrypoint-initdb.d).
make up

# 2. Run the test suite.
make test          # unit + integration + concurrency + regression

# 3. Run the server.
make run           # listens on :8080
```

Hit it:

```bash
# Seed two wallets first (no public endpoint; use psql).
docker compose exec postgres psql -U wallet -d wallet_transfer -c \
  "INSERT INTO wallets (id, balance) VALUES ('w1', 1000), ('w2', 0);"

# Make a transfer.
curl -X POST http://localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: key-001' \
  -d '{
        "fromWalletId": "w1",
        "toWalletId":   "w2",
        "amount":       100
      }'

# Retry with the same key — same response, no duplicate effect.
curl -X POST http://localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: key-001' \
  -d '{
        "fromWalletId": "w1",
        "toWalletId":   "w2",
        "amount":       100
      }'

# Read the wallet balance.
curl http://localhost:8080/wallets/w1

# Read the transfer history (paginated; ?limit=N&before=RFC3339).
# Get the 20 most recent transfers for w1
curl "http://localhost:8080/wallets/w1/transfers?limit=20"

# Get the next 10 transfers before timestamp 2026-05-20T10:30:00Z
curl "http://localhost:8080/wallets/w1/transfers?limit=10&before=2026-05-20T10:30:00Z"
```

---

## Project structure

```
wallet-transfer-service/
├── cmd/server/main.go              # entry point: wires pool → repos → service → handler → HTTP
├── internal/
│   ├── domain/                     # PURE: entities, state machine, validation. No I/O.
│   │   ├── wallet.go               # debit/credit invariants (I4)
│   │   ├── transfer.go             # state machine: PENDING → {PROCESSED, FAILED} (I7)
│   │   ├── ledger.go               # entries (debit/credit pair per transfer)
│   │   └── errors.go               # sentinel errors mapped to HTTP codes at the handler
│   ├── repository/                 # SQL only. Methods accept pgx.Tx; never open their own.
│   │   ├── postgres.go             # pool + error-class detection (unique/FK/check violations)
│   │   ├── transfer_repo.go        # INSERT, SELECT FOR UPDATE, conditional UPDATE
│   │   ├── wallet_repo.go          # sorted FOR UPDATE locking (Race prevention)
│   │   └── ledger_repo.go          # double-entry pair insertion
│   ├── service/                    # business logic, orchestration, idempotency, transactions
│   │   ├── transfer_service.go     # T1 / T2 / REPLAY algorithm (§7 of DESIGN.md)
│   │   └── hash.go                 # canonical payload hash for I9 (key-reuse detection)
│   └── handler/                    # thin HTTP: validation + transport mapping. No logic.
│       ├── transfer_handler.go     # POST /transfers + Read endpoints + error→status code mapping
│       └── server.go               # chi router + middleware
├── migrations/001_init.sql         # schema, translation of DB_DESIGN
├── tests/                          # integration + concurrency tests against real Postgres
│   ├── helpers_test.go             # DB setup, seed fixtures, assertInvariants() helper
│   ├── integration_test.go         # happy path, idempotency replay, 409, 404, validation
│   └── concurrency_test.go         # I4 overdraft, I8 idempotency race, crossing transfers
├── docker-compose.yml              # local Postgres for dev + tests
├── Makefile                        # up / down / test / run
├── SYSTEM_DESIGN_DOC.md            # system design doc (read this first)
├── SYSTEM_INVARIANTS.md            # system invariants (read this for the spec of correctness)
└── go.mod
```

The architecture follows the brief's prescribed layering strictly:
- **Handler** = request validation, transport mapping, invoking service. No business logic.
- **Service** = business logic, orchestration, idempotency behavior, transfer workflow. Owns transaction boundaries.
- **Repository** = persistence operations, database interaction. Accepts `pgx.Tx`; never opens its own.
- **Domain models** = entities, state transitions, validation rules. Pure Go, no I/O.

---

## The algorithm in one screen

For the full reasoning see §6–§9 of `SYSTEM_DESIGN_DOC.md`. The implementation in `internal/service/transfer_service.go` is a direct translation.

```
POST /transfers
  │
  ▼
T1 (own tx) — INSERT transfers (..., status='PENDING')
  │
  ├── UNIQUE(idempotency_key) violation ──► REPLAY (see below)
  ├── FK violation on wallet                ──► 404
  └── success → COMMIT T1
                  │
                  ▼
                T2 (own tx)
                  • SELECT * FROM transfers WHERE id=? FOR UPDATE
                  • SELECT * FROM wallets WHERE id IN (?,?) ORDER BY id FOR UPDATE
                  • If insufficient funds: MarkFailed, UPDATE transfer, COMMIT → return FAILED (201)
                  • Else: insert ledger pair, update both balances, MarkProcessed,
                          UPDATE transfer (WHERE status='PENDING'), COMMIT → return PROCESSED (201)

REPLAY (own tx)
  • SELECT * FROM transfers WHERE idempotency_key=? FOR UPDATE
  • payload_hash mismatch        → 409
  • status terminal              → return cached transfer (pure read, no writes)
  • status PENDING               → run T2 IN THIS SAME TX (we hold the row lock),
                                    then COMMIT
```

Why this is exactly-once at the API:
1. `UNIQUE(idempotency_key)` ensures only one request can ever proceed past T1 to run T2. All duplicates land in REPLAY, which is a pure read of an immutable terminal record.
2. `UNIQUE(transfer_id, direction)` on the ledger ensures even an internal re-execution (recovery worker, REPLAY-runs-T2) cannot post the same legs twice.

---

## Tests

```bash
make test-unit          # pure domain tests, no DB
make test-integration   # happy path, replay, 409, validation
make test-concurrency   # the two highest-leverage tests:
                        #   - I4: 10 parallel transfers from a wallet with insufficient
                        #         total balance — exactly 5 succeed, 5 fail, no overdraft
                        #   - I8: 50 parallel calls with same key+body — exactly 1 transfer,
                        #         1 ledger pair, all responses identical
                        #   - Crossing transfers A→B/B→A — no deadlock
                        #   - REPLAY runs T2 on stranded PENDING
                        #   - Recovery worker picks up stranded PENDING
make  test-regression   # a regression test for a regression caught bug — see tests/regression_test.go
```

Every concurrency test ends with `assertInvariants()`, which verifies:
- **I1** — every PROCESSED transfer has matching debit/credit amounts.
- **I3** — `wallets.balance == latest ledger_entries.balance_after` for each wallet.
- **I4** — no negative balances.
- **I6** — PROCESSED transfers have exactly 2 ledger entries; FAILED have 0; PENDING have 0.

If any concurrency test commits a corrupt state, the invariant check fails the test regardless of what the test itself was asserting.

---

## Schema notes

See `migrations/001_init.sql` for the implementation DDL.

**Decisions worth knowing:**

- Money is `BIGINT` minor units. No floats.
- Status and direction are Postgres `ENUM`s, not `VARCHAR + CHECK`.
- `ledger_entries` has both `UNIQUE (transfer_id, direction)` and `UNIQUE (transfer_id, wallet_id)` — the first makes "exactly one DEBIT + one CREDIT per transfer" a structural invariant.
- `wallets.balance` and `ledger_entries.balance_after` both have `CHECK >= 0` as the final backstop on I4/I5.
- Partial index `ix_transfers_pending` serves the recovery worker — only PENDING rows are indexed.
- `idempotency_records` is merged into `transfers` as a `UNIQUE` column. The brief suggests a separate table; we merged because for a single-endpoint service the key maps 1:1 to a transfer aggregate and the separate table would carry only foreign keys. Rationale is in `SYSTEM_DESIGN_DOC.md` §9.

---

## Concurrency strategy

Pessimistic row-level locks (`SELECT ... FOR UPDATE`) at `READ COMMITTED` isolation, with **deterministic lock order** on wallet rows by ascending ID. The wallet repository sorts internally, so callers cannot accidentally break the invariant.

This eliminates the four race conditions enumerated in §8 of `SYSTEM_DESIGN_DOC.md`:
1. **Duplicate idempotency key** → `UNIQUE` constraint serializes T1.
2. **Concurrent drains on one wallet** → `FOR UPDATE` on wallet serializes; balance CHECK as backstop.
3. **Crossing transfers (A→B, B→A)** → sorted lock order eliminates deadlock.
4. **Retry handler vs. recovery worker** → `FOR UPDATE SKIP LOCKED` on the recovery side; status re-check under lock in both.

`READ COMMITTED` is sufficient because every contended read is an explicit `FOR UPDATE` — there are no "phantom read" risks for this workload.

---
## AI usage:
I used Claude code, extensively not just for implementation but also during the research for it, along with tech-docs from Stripe, Square, and 
some blogs on Notion on how they implement transfers. 
I did use GitHub CoPilot and Claude for code generation, but it was mostly around boilerplate code, write tests, logs and adding descriptive comments
after the research on the design doc was locked in. Following items were not AI generated:
1. The architectural choice of pessimistic locking vs. optimistic locking.
2. The choice to merge idempotency_records into transfers.
3. The choice to drop currency, Make the DB design minimal to focus on the core transfer algorithm.
4. The Idempotency-Key-as-header decision. And how to use it for both replay detection and payload hash storage without a separate table.
5. The first round of implementation of wallet/transfer handler, service, repository, and domain design. 
6. The TDD style of development.

Following things were AI generated:
1. SQL queries, after the DB schema was designed and locked in. 
2. The consecutive round of implementation, after the design doc was locked in and the first round of implementation was done.
3. Tests, after the test cases were thought through and locked in.
4. The verbose README and explaination of the Design/Invariant Doc, after the core design and implementation were locked in.

## Out of scope
As per the brief's "focus on correctness and clarity, not feature completeness" — see §17 of `SYSTEM_DESIGN_DOC.md` for the full list. Highlights:

- Wallet creation endpoint (wallets are seeded via SQL in this submission).
- `GET /transfers/{id}` (Optional Enhancement, not implemented).
- Multi-currency. (Can be added by adding a `currency` column to `wallets`, `transfers`, and `ledger_entries`, and enforcing currency consistency in the service layer.)
- Transactional outbox for webhooks.
- Auth, rate limiting.
- Production observability (designed in §14 of `SYSTEM_DESIGN_DOC.md`, not built).
- Comprehensive reconciliation and drift monitoring against the ledger is out of scope; that would be a production addition.
- Error handling, and logging is minimal and not structured and at places gives out implementation details (e.g. "T2 invariant violation") — this is intentional to keep the focus on the core algorithm and not on production readiness.