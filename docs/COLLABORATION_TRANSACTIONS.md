# Collaboration transaction contract

The authoritative repository is in cloud PostgreSQL. CLI `.cxt` directories are
working replicas with durable delivery records; they are not the shared database.
These guarantees apply to the PostgreSQL adapter and upgraded clients. The
filesystem backend is a single-machine development adapter and does not provide
multi-file or multi-server ACID transactions.

## What commits together

| Operation | Atomic publication boundary |
|---|---|
| Object batch | Validated document bodies, repository ownership, snapshot rows, indexes and transactional storage accounting |
| Ref batch | All requested ref changes, graft overlays, reflog entries, associated pending resolution and notification jobs |
| Branch history, fork, join | History/retention evidence and the resulting shared refs and overlays |
| PR promotion | Exact source binding, DAG append, ref/reflog, completion receipt, and the claimed job's completed state |
| Pending replacement | Pointer replacement and verified obsolete-prefix cleanup; distinct raw captures are retained |
| Memory publication | Immutable body first, then atomic compare-and-swap of the snapshot's memory pointer; a losing body's data is retained |
| Encrypted secrets | Replace the exact editing revision and enqueue its notification in the same transaction |
| New member accepts invite | Membership change and notification; concurrent acceptance creates one event |

Chunk uploads and memory bodies can exist before their publication pointers.
Such staging objects are deliberately retained and are not an incomplete graph
commit. Separate HTTP requests, separate repositories, Git, provider files and
the cloud database do **not** form one distributed database transaction.

## Isolation and conflicts

Repository business writes acquire a PostgreSQL transaction-scoped advisory lock
before reading mutable graph state. Same-repository operations are serialized
across server instances; different repositories can proceed independently.
Row locks, unique constraints and existing CAS predicates still apply to
individual registers. Direct SQL writes that bypass these application rules are
outside this contract.

All participating adapters use the connection bound to the operation's context.
Nested store transactions are savepoints and cannot commit the outer operation.
Passing that context to another store instance fails rather than opening an
independent write. Transaction callbacks must not start goroutines or make
network calls.

- A supplied `expected_target` is checked even for a forced ref update. An
  identical target replay succeeds. Omitting the expectation retains the
  existing explicit, unconditional force semantics.
- Memory CAS permits one writer at a given parent revision. Both immutable
  bodies survive a conflict so the caller can reconcile them.
- Pending pointers are serialized last-accepted captures, not ordered by
  transcript length: a provider compaction can legitimately shorten a session.
  Dismissal is sticky and does not delete session data.
- PR workers lock and validate the claimed job's version, state and unexpired
  lease **before** graph mutation. A superseded worker cannot publish. The job
  row stays locked through the final commit, preventing a second claim while
  that valid worker finishes.
- Registration can fill an unknown Git origin, but cannot overwrite an
  established origin or reset the configured default branch.
- Secrets require the `expected_revision` returned before editing, even for
  same-passphrase edits or rotation. First creation uses `absent`. A missing
  baseline returns 428; a stale baseline returns 409 `secrets_conflict`.
  Every accepted write receives a fresh opaque revision, including identical
  ciphertext writes, preventing an ABA overwrite. Legacy reads receive a stable
  revision without rewriting their ciphertext. Upgrade all writers together.
- The web editor retains its original baseline across query refreshes and
  reopening. Conflicts preserve the draft; loading latest retains prior drafts
  for comparison. CLI baselines are scoped to server, repository and worktree.
  A push without a baseline can create an absent envelope but cannot replace an
  existing one. A failed push never changes local plaintext or its baseline.

Only explicit database transaction-abort codes (`40001`, `40P01`) receive up to
three attempts, each with a fresh transaction and fresh after-commit callbacks.
An uncertain COMMIT acknowledgement is not blindly retried. Durable operation
IDs, PR completion receipts and CAS make operation-specific reconciliation
possible after reconnecting. A completed PR replay never reapplies an old merge
over a later deliberate rewind.

## Authorization belongs to the write transaction

Authenticated HTTP requests carry an application actor. Each repository command
rechecks the current direct, organization and team role after entering the same
PostgreSQL transaction that mutates data. Archived repositories reject writes;
management and team-default writes also recheck their operation-specific policy.
Missing identity fails closed. Verified host webhooks and internal workers use
an explicit system identity, never a caller-supplied system flag.

Body decoding happens before the transaction. A request paused while uploading
cannot use a role revoked before its write starts. Conversely, identity changes
wait for an already-authorized transaction to finish. Notification retries use
the identity transaction boundary as well. Filesystem development storage
rechecks current authority but does not promise PostgreSQL cross-process ACID.

## A graph read is one generation

`GET /api/v1/repos/{repoID}/view` reads refs, snapshots, reflog, history, pending
and unsync metadata in one read-only REPEATABLE READ transaction, with the same
viewer permission boundary as the existing graph APIs. Each response therefore
contains one committed database generation, even while another server appends
a PR. One failed component fails the whole response.

Context and On Hold use one React Query cache entry for this response. Revision-driven refresh
replaces the complete bundle; it cannot combine new refs with old snapshots or
completion receipts. Document bodies remain fetched separately by immutable
hash. Memory projections, manifests, pull responses and integrity audits also
use a consistent database read snapshot.

## Durability and process failure

The PostgreSQL pool requests `synchronous_commit=on` (preserving the stronger
`remote_apply` setting) and refuses a server with `fsync` or `full_page_writes`
disabled. A request cancellation or lost connection rolls back an uncommitted
operation, including changes inside released savepoints. Rollback uses an
independent bounded cleanup context.

CLI mutation locks use a persistent-inode OS file lock. They cannot expire while
a writer is alive; the kernel releases the lock when the process exits. The
legacy directory lock remains during upgrades, and a dead owner's lock can be
recovered immediately. Upgrade all simultaneous local writers for this guarantee.
Supported systems are macOS and Linux with local filesystems; network filesystem
locking and local disk durability require separate operator validation.

Ref updates, secrets changes and new invite acceptances enqueue a PostgreSQL
notification outbox record in the same transaction as their business change.
An enqueue failure rolls the change back. A worker claims committed jobs with
`FOR UPDATE SKIP LOCKED`, a version and a two-minute lease; it sends outside
transactions with a five-second HTTP timeout. Database time governs claims and
lease fencing across replicas. Reopening the process retains pending work.

Delivery is **at least once**, not exactly once: a receiver can accept an event
before the worker loses the response or crashes before recording success.
`X-CXTHub-Event-ID` remains stable for every retry; compatible receivers can use
it for deduplication. Only 2xx acknowledges delivery. Redirects are not followed.
Network failures, 408, 425, 429 and 5xx retry with exponential delay (30 seconds
initially), up to eight attempts. `Retry-After` is respected up to 24 hours.
Other failures or exhausted attempts require attention.

Repository settings show the latest 100 jobs, safe failure reasons and retry
controls for maintainers/owners. Webhook credentials and secret values are never
included in status. A disabled, archived or changed destination stops automatic
sending once observed by a worker. An already in-flight request cannot be
recalled. Explicit retry uses the currently saved webhook with the same event ID;
active leases and delivered events cannot be manually replayed. Historical jobs
remain stored; the UI limit is not a retention policy.

The filesystem development adapter persists individual queue transitions but
cannot atomically combine separate business and queue files. PostgreSQL remains
required for the production transaction guarantee.

PostgreSQL durability still depends on honest storage/fsync, backups and tested
restore procedures. Surviving primary-machine loss without acknowledged-write
loss additionally requires an appropriate synchronous replica/failover policy;
setting `synchronous_commit=on` alone does not provision replicas. See PostgreSQL
[WAL settings](https://www.postgresql.org/docs/16/runtime-config-wal.html),
[locking](https://www.postgresql.org/docs/17/explicit-locking.html), and
[concurrency control](https://www.postgresql.org/docs/16/mvcc.html).

## Regression evidence

CI runs real PostgreSQL tests, including repeated Go race-detector runs:

- Two separate server pools limited to two connections each, six parallel PRs with duplicate deliveries, and a
  concurrent ordinary push: every source remains reachable, one completion per PR.
- Invalid second ref update, failed second snapshot, failed completion receipt,
  and failed job completion: no partial publication.
- Stale worker claims, explicit rewind followed by old PR replay, stale forced
  writes, concurrent memory CAS and encrypted-envelope CAS.
- Cancellation and backend connection termination: rollback; reopening the
  store sees the committed state.
- A PR commits between the graph reader's refs and snapshot reads: the reader
  returns the old complete generation, and the next request returns the new one.
- CLI writer termination and an artificially aged live lock: recovery without
  stealing a live writer's lock.
- Injected outbox insert failure rolls back ciphertext, refs and membership;
  concurrent invite acceptance through independent PostgreSQL pools creates one event.
- Same-key edit and rotation conflicts, dirty CLI pulls, worker lease takeover,
  restart persistence, transient HTTP failures and destination changes.
- Browser E2E: stale drafts survive rejection and latest-version comparison;
  rotation sends its decrypted revision; delivery status and retry controls work.
- Browser E2E: graph polling advances from the complete view even if the legacy
  component endpoints still expose an older generation.

These are explicit guarantees for the tested paths, not a claim that every
future implementation or infrastructure failure is impossible.
