# Org-space MVCC atomicity regression fix

Date: 2026-07-12

Commit: this report and the implementation land together in `fix: serialize org space event mutations`.

## Root cause

`ApplyOrgSpaceEvent` used one PostgreSQL data-modifying CTE for the knowledge-space row, binding, version snapshot, member upserts, and stale-member deletion. A concurrent v3 statement acquired its statement snapshot before blocking on v2's `knowledge_spaces` unique conflict. After v2 committed, v3 could update the conflicted knowledge-space row, but the rest of the same CTE retained the earlier snapshot. Consequently, v3's stale-member deletion could not see the newly committed v2 member and left a mixed-version membership set.

The unique conflict serialized one row mutation; it did not provide a fresh snapshot for related-table reads later in the statement.

## RED evidence

The PostgreSQL concurrency test was changed from fixed sleeps to deterministic coordination:

- a test connection holds an advisory lock;
- the v2 binding trigger blocks on that lock;
- the test waits for PostgreSQL to report the first lock waiter;
- v3 starts, and the test waits for a second lock waiter before releasing v2.

Against the unsafe CTE, the focused test failed with:

```text
inconsistent atomic state: version=3 parent=company:parent-v3 versions=3 members=[v2-member v3-member]
```

## Fix

`ApplyOrgSpaceEvent` now runs a real PostgreSQL transaction:

1. Acquire a transaction-scoped advisory lock keyed by enterprise ID plus org scope.
2. Read the knowledge space in a separate READ COMMITTED statement after acquiring the lock.
3. Reject same-version or stale events without mutation.
4. Insert or strictly-newer update the space.
5. Write the typed org binding and immutable version snapshot.
6. Upsert normalized members using typed sqlc parameters.
7. Delete members from older versions in a subsequent statement.
8. Commit all effects together, or roll back all effects on any error.

This preserves initial-create race safety, strict version ordering, retry after rollback, parent FK rollback, typed parent identity, and normalized member behavior. Hash collisions can only serialize unrelated scopes; they cannot weaken correctness.

## GREEN evidence

- Focused PostgreSQL atomicity test: PASS, including initial failure/retry, post-space FK failure/retry, concurrent v2/v3, and stale replay.
- Deterministic concurrent PostgreSQL subtest: PASS with `-count=10`.
- Full atomicity test: PASS with `-count=3`.
- `go test ./internal/spaces ./tests/integration -count=1` with the real PostgreSQL DSN: PASS.
- Pinned `golang:1.26-bookworm` Linux container, `go test -race` focused internal/spaces tests: PASS.
- Pinned Linux container, real-PostgreSQL `go test -race ./tests/integration -run '^TestOrgSpaceEventAtomicityPostgres$' -count=1`: PASS.
- Pinned Linux container, DSN-enabled `go test ./... -count=1`: PASS, including `tests/integration`.
- `sqlc generate`: PASS and reproducible.
- `go vet ./...`: PASS.
- `git diff --check`: PASS.

Windows-native `-race` was unavailable because the host has no C compiler; the pinned Linux container supplied the race evidence instead.

## Self-review

- Scope is limited to the org-space SQL query surface, the transaction implementation, and the atomicity regression test.
- No Task 4 Dream/workflow code, migrations, public contracts, dependencies, or alternate storage paths changed.
- The advisory lock is acquired before any scope-state read and is held until commit/rollback.
- Every related-table mutation uses the same transaction while separate statements obtain fresh READ COMMITTED snapshots.
- Failure paths return through the existing transaction helper, whose deferred rollback preserves same-version retry behavior.
- Generated sqlc output matches the checked-in query source.
- No enterprise-only material, secrets, private endpoints, or customer data were added.

## Review follow-up

Review found that the first fix relied on the deployment/session default being READ COMMITTED and that its test barrier counted unrelated database waiters.

### Isolation RED

A new real-PostgreSQL regression sets one operation connection's `default_transaction_isolation` to `repeatable read`, starts v2 while its scope advisory lock is held, commits a complete v1 state from another transaction, and then releases the lock. Before the follow-up fix, the operation retained its pre-lock snapshot and failed as expected:

```text
operation inherited repeatable-read session default: apply org space event department:isolation-child v2: ERROR: duplicate key value violates unique constraint "knowledge_spaces_enterprise_id_org_scope_key" (SQLSTATE 23505)
```

### Follow-up fix

- Added an options-aware transaction helper using `BeginTx`.
- `ApplyOrgSpaceEvent` now explicitly begins with `pgx.TxOptions{IsoLevel: pgx.ReadCommitted}` regardless of the session or deployment default.
- The regression resets the session setting before returning its connection to the pool.
- The concurrency test now assigns dedicated connections to v2 and v3, captures their PostgreSQL backend PIDs, and waits for exact advisory-lock waiter/holder pairs in `pg_locks`. It no longer observes global lock-waiter counts.

### Follow-up GREEN evidence

- Repeatable-read-default regression plus PID-specific concurrency test: PASS.
- Pinned Linux container, real PostgreSQL, both reviewed subtests with `-count=10`: PASS (`2.675s`, exit 0).
- Focused unit and complete PostgreSQL atomicity test: PASS.
- Pinned Linux container, `-race` focused `internal/spaces` plus real-PostgreSQL atomicity: PASS (`internal/spaces 1.023s`, `tests/integration 1.453s`, exit 0).
- Pinned Linux container, DSN-enabled `go test ./... -count=1`: PASS, including `tests/integration` (`9.210s`, exit 0).
- `sqlc generate`: PASS and reproducible.
- `go vet ./...`: PASS.
- `git diff --check`: PASS.

Follow-up self-review confirmed that the isolation override is local to this operation, unrelated transaction users keep their existing behavior, advisory waits are identified by exact backend PIDs and matching advisory lock identity, and no unrelated Dream/workflow code changed.
