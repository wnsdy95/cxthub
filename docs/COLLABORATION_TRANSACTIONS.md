# Collaboration transaction contract

The authoritative repository is in cloud PostgreSQL. A repository's `.cxt`
folder on a member's computer holds the CLI's connection settings and copies
of what it uploaded; it is not the shared database.

These guarantees apply to the PostgreSQL adapter and upgraded clients. The
filesystem backend is a single-machine development adapter and does not provide
multi-file or multi-server ACID transactions.

## What commits together

| Operation | Atomic publication boundary |
|---|---|
| Conversation document | The verified document, the repository's ownership of it, its read index, the completion of its finalization job and transactional storage accounting |
| Session report | The report, the index from commits to its segments, and the ranges that the report opens; the role of the caller is rechecked in the same transaction |
| Session removal | The report with its commit links, its summaries and its open ranges, the remembered removal, and the deletion of the conversation document when no other report uses it |
| Delivered push | The branch ledger observation and the request for a read of the remote |
| Encrypted secrets | Replace the exact editing revision and enqueue its notification in the same transaction |
| New member accepts invite | Membership change and notification; concurrent acceptance creates one event |

Uploaded chunks can exist before the document that uses them is published.
Such staged objects are deliberately retained and are not an incomplete
publication. A session's document and its report are stored by separate
requests: the report is taken only when the repository holds the document it
names. Separate HTTP requests, separate repositories, Git, the tools' own
files and the cloud database do **not** form one distributed database
transaction.

## Isolation and conflicts

Repository business writes acquire a PostgreSQL transaction-scoped advisory lock
before reading mutable repository state. Same-repository operations are serialized
across server instances; different repositories can proceed independently.
Row locks, unique constraints and existing CAS predicates still apply to
individual registers. Direct SQL writes that bypass these application rules are
outside this contract.

All participating adapters use the connection bound to the operation's context.
Nested store transactions are savepoints and cannot commit the outer operation.
Passing that context to another store instance fails rather than opening an
independent write. Transaction callbacks must not start goroutines or make
network calls.

- A session belongs to the account that first reported it. Another account's
  report for the same session is refused with a conflict. Storing the same
  report again changes nothing; otherwise the report's revision increases.
- No report is taken for a removed session, from anyone, until the removal is
  taken back.
- A provider delivery that is processed twice changes the branch ledger once.
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
An uncertain COMMIT acknowledgement is not blindly retried. Every write of the
session structure can be repeated: a report or a summary with the same content
again changes nothing.

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

## Durability and process failure

The PostgreSQL pool requests `synchronous_commit=on` (preserving the stronger
`remote_apply` setting) and refuses a server with `fsync` or `full_page_writes`
disabled. A request cancellation or lost connection rolls back an uncommitted
operation, including changes inside released savepoints. Rollback uses an
independent bounded cleanup context.

Secrets changes and new invite acceptances enqueue a PostgreSQL
notification outbox record in the same transaction as their business change.
A branch that moves sends no notification.
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

Every pull request runs these real PostgreSQL tests once; the `CI heavy` workflow
(main pushes, nightly, `ci:full` pull requests) repeats them under the Go race detector:

- Cancellation and backend connection termination: rollback; reopening the
  store sees the committed state.
- Injected outbox insert failure rolls back ciphertext and membership;
  concurrent invite acceptance through independent PostgreSQL pools creates one event.
- Same-key edit and rotation conflicts, dirty CLI pulls, worker lease takeover,
  restart persistence, transient HTTP failures and destination changes.
- Browser E2E: stale drafts survive rejection and latest-version comparison;
  rotation sends its decrypted revision; delivery status and retry controls work.

These are explicit guarantees for the tested paths, not a claim that every
future implementation or infrastructure failure is impossible.

## Authorization and audit completion (2026-09-24)

About and repository config edits validate and commit as one
command. Organization metadata audit is appended inside ordinary repository,
settings, secrets and membership write transactions; audit failure rolls the
write back. Internal system workflows retain their dedicated history/outbox
records rather than impersonating a human actor. `X-Request-ID` correlates REST
operations with audit entries; it never grants authority or idempotency.

Organization administrators can page `/organizations/{id}/audit/page` with a
`(created_at, id)` cursor. The organization is bound into the cursor and current
access is checked on every request. `/audit/export` exports one page as JSONL;
follow `X-Next-Cursor` for older pages. The UI explicitly exports loaded records.
No plaintext settings, webhook credentials or secret envelopes enter this log.

### Reproducible validation

Use only disposable PostgreSQL 16 databases. Do not point fixtures at production.

- `CXT_TEST_DSN=… go test -tags postgres ./internal/adapters/store -timeout=120s`
  uses fixed smoke fixtures and must run once against a fresh database.
- `CXT_TEST_DSN=… go test -tags postgres ./internal/app ./internal/adapters/delivery/http -run '^TestPG' -race -count=3`
  checks transactions, rollback, revocation, invitations and MCP token races.
- From the repository root, `CXT_RECOVERY_DSN=… CXT_RECOVERY_RESTORE_DSN=… bash scripts/verify-postgres-recovery.sh`
  backs up and restores into an explicitly empty target, then compares every
  public table's row count and sorted row-content digests. After equality is
  verified, it calls `cxt_reset_catalog` for every restored repository in ID
  order within one transaction. Before committing, it checks that every epoch
  differs from the source and every repository has a complete generation-zero
  baseline. It then verifies that the source is unchanged and that all restored
  tables except the two catalog tables still match. It refuses a nonempty target;
  temporary dumps are permission-restricted and removed afterwards.

Keep source writers stopped on both validation databases
throughout the rehearsal. Exact backup equality must be verified **before**
rotating restored epochs, and rotation must commit **before serving** the restored
database. Otherwise, work accepted before recovery could incorrectly appear
current after a database rewind. For an older schema without `cxt_reset_catalog`,
the script reports `catalog epoch rotation not available; migrate before serving`;
the content comparison alone does not validate catalog recovery. See
[Catalog maintenance and restore](CATALOG_SYNC.md#maintenance-retention-and-restore)
for epoch rotation.

An upgrade rehearsal started the previous `c5d17aa` PostgreSQL binary against an
empty database, created an Organization, repository, member, Team and maintainer
grant through HTTP, and restarted with this revision. Migrations 0056–0060 applied
once; a second restart applied zero. Repository/namespace IDs, owner and member
access, Team membership/grants and the existing URL were unchanged; base access
defaulted to none. This is a forward migration check, not a downgrade guarantee.

## Invitation email delivery

Migration `0061` adds an invitation email outbox. A new or renewed invitation,
its audit event and its frozen email payload commit in one PostgreSQL identity
transaction. Failure to queue rolls them all back. The FS development adapter
uses atomic files and in-process command serialization; it does not promise
multi-file crash rollback or coordination between separate server processes.

Workers claim jobs and recheck invitation expiry, status and issuer authority
under the identity transaction, then release it before network IO. A two-minute
lease and attempt fencing prevent old workers from overwriting newer outcomes.
Provider requests time out after ten seconds. All replicas use the database
clock for claims. Revocation before claiming suppresses delivery. Revocation
after a send begins cannot recall an email, but its link still cannot confer
membership: acceptance checks current invitation/issuer/recipient state.

The email payload and `collaboration-invite/<invitation-id>` idempotency key
remain unchanged across restarts and configuration edits. Transport failures,
408/425/429/5xx and an in-flight idempotent request retry with bounded backoff,
respecting Retry-After. Other provider rejections need attention. There are at
most twelve attempts, and none begin at or after 23 hours from the first claim,
inside Resend's [24-hour deduplication window](https://resend.com/docs/dashboard/emails/idempotency-keys).
A request with a lost response may already have sent mail; do not claim
unconditional exactly-once delivery across a provider retention boundary.
Manual renewal revokes the old invitation and creates a new ID and mail request.

API/UI status separates invitation acceptance from email state (`queued`,
`sending`, `retrying`, `accepted` by Resend, `attention`, `cancelled`, `not_sent`).
No delivery webhook is configured, so inbox delivery and bounce events are not
asserted. No key or raw provider error response is persisted or returned. The
outbox contains recipient and body data and belongs inside the same protected
backup/access boundary as invitations. Keep the same Resend account for queued
retries; account migration requires reconciling in-flight requests first.

During a rolling upgrade, only upgraded instances enqueue emails. Upgrade every
instance before enabling Resend on any of them. Do not downgrade to a version
without this worker while there are unresolved sends; pending jobs remain
persisted but will pause. Existing invitations are not automatically emailed.
