# Background jobs

The API supports a PostgreSQL-backed job queue. Only operations explicitly enqueued through the job service run asynchronously; registering a definition does not automatically move an HTTP workflow to the queue. Durable storage allows multiple API instances to share jobs and preserves queued work across process restarts.

## Storage and ownership

The `background_jobs` table stores job identity, owner and selected-repository IDs, an optional engineering-run ID, the registered job type, JSON reference payload/result, status, priority, idempotency key, attempt counters, scheduling and lifecycle timestamps, a coarse error category/reference, and the active lease owner/expiry.

The payload and result size limits default to 64 KiB each. Job payloads should contain identifiers and small control values only. Do not put GitHub tokens, credentials, source-file contents, raw AI responses, or arbitrary exception text in job records. Error storage is intentionally limited to a coarse category and a bounded stable reference.

API reads and user-triggered mutations are scoped by authenticated user and selected repository. The API resolves repository ownership before looking up a job, so a foreign job is indistinguishable from a missing job. Worker claiming and retention are system-wide queue operations across owners.

## Claiming and recovery

Workers claim one ready job inside a database transaction using `SELECT ... FOR UPDATE SKIP LOCKED`. The claim updates the status, increments the attempt count, and sets a worker ID and lease expiry in the same transaction. Two workers sharing PostgreSQL therefore cannot claim the same row at the same time.

The default lease is two minutes. A live worker renews it every third of the lease interval. If the process exits or loses its lease, a later claim transaction detects the expired lease and moves the abandoned job back to the retry queue. If the attempt budget has already been exhausted, the job becomes a terminal failure. An interrupted worker never writes a stale result after another worker has acquired the row because final updates are fenced by the lease owner.

An explicit owner cancellation is distinct from process shutdown. A running cancellation is cooperative and transitions through `cancel_requested`; shutdown leaves the lease to expire so another worker can recover the work.

## Retries, idempotency, and retention

- Definitions provide the default attempt count and optional error classifier. A caller may override the default attempt count for a specific enqueue through `EnqueueOptions.MaxAttempts`. The worker applies exponential retry backoff (five seconds initially, capped at five minutes by default) only to retryable categories.
- Idempotency keys are unique per user and selected repository. Repeating an enqueue with the same non-empty key returns the original job rather than creating another.
- Failed jobs can be explicitly retried by their owner. Retry resets the attempt counter and schedules a fresh execution.
- Succeeded, failed, and cancelled records are retained for 30 days by default; the worker periodically prunes older terminal records. Active and retrying work is never pruned.

## Tests

The in-memory store remains available for isolated unit and HTTP-handler tests. Production wiring uses `NewPostgresStore` and runs `jobs.Migrate` before starting the worker.

Run the API unit and race tests from `services/api`:

```sh
go test -race ./internal/jobs -count=1
```

The PostgreSQL integration test is opt-in. Provision a fresh, disposable PostgreSQL database named with the `_test` suffix (for example `aevor_jobs_test`) before running it. The test refuses to run against any database name without that suffix; it does not create or clean up a database:

```sh
export AEVOR_TEST_DATABASE_DSN='host=localhost port=5433 user=aevor password=aevor dbname=aevor_jobs_test sslmode=disable'
go test ./internal/jobs -run TestPostgresStore_DurabilityIdempotencyAndConcurrentClaims -count=1
```

The integration test only deletes rows carrying its own randomly generated test user ID. Do not point it at the API development database or any shared production database.
