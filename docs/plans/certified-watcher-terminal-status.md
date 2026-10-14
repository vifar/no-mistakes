# Certified watcher terminal status

An explicit operator cancellation or controlled planned daemon shutdown may
stop an unchanged CI watcher without replacing its already posted validation
success. The run records its truthful terminal reason. Preservation sends no
new status request.

The exception requires current publication ownership, a freshly verified clean
terminal head equal to the last pushed and successful head, and fresh exact-head
Review, Test, Document, and Lint completion without skips or exceptions. Durable
proof binds validation epoch, configuration, step execution, and push generation.
CI-only readiness cannot issue this proof.

Repair entry and reset invalidate old proof before tools. Failed/no-op repairs
cannot revive it. Renewal requires all four gates anew and a new durable success
POST. Every non-duplicate replacing POST first revokes prior eligibility;
uncertain requests and failed ledger saves leave it absent.

Terminalization records preservation atomically with the outcome. Every callback
reads that decision before promotion, deduplication, or posting. Recovery consumes
only committed decisions. Missing proof, crashes, deadlines, supersession,
independent errors, and incomplete shutdown preparation fail closed.

Planned stop and managed refresh share instance-bound preparation. Record the
reason before cancellation, drain terminal decisions before acknowledgement,
close the client before waiting, and complete supervisor cleanup. Wait and PID
fallback name only the captured process. Pending intent cannot authorize a later
crash or raw signal.

Implementation and evidence:

1. Red cancellation/error-race journeys with real temporary Git and database
   state and an HTTP request oracle.
2. Nullable strict-success certificates and fresh per-step execution bindings.
3. Repair/reset invalidation and revocation before every replacing POST.
4. Atomic terminal decisions and normalization of every late callback.
5. Controlled shutdown and exact-instance stop/refresh handling.
6. Uncertainty, recovery, owner/config/head changes, repair renewal, and successor
   races; handbook update and independent implementation review.

Use owning Go package tests, focused race checks, and the supported full shipping
pipeline. No manual status repair, runtime installation, or deployment belongs
to this source change.

The database wrapper exposes no storage-recovery notification. Failed raw
supersession, terminal writes and terminal readback share one queue-owned recovery
continuation. Failed work stays queued until it succeeds; readback retries never
repeat a committed terminal write. Closing the database ends retries on that
closed handle with an explicit error and retained manager custody. It checks
again at one-second intervals using the existing bounded publication-lock
attempt and a five-second raw-supersession transaction deadline, stops when its owned queue drains, and keeps raw supersession ahead of
terminal decisions throughout recovery. Executors keep custody until their
settlement result arrives; recovery never needs another caller to acquire the
lock. Process loss discards the continuation, so startup invalidates unfinished
proof and settles obsolete runtime intent rather than treating it as authority.
Interrupted CI retains its display state while its status ledger and outbound
reporting reconcile to failure.
