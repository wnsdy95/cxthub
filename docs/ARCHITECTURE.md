# Architecture and enforced boundaries

CXTHub stores shared repository facts in the cloud backend. The CLI's `.cxt`
store is a local replica, recovery outbox and worktree selection cache. It is not
the authoritative database for Web or remote MCP queries.

## Ownership

- **Domain:** immutable history/evidence values, branch identities, applicability,
  Join planning and secrets policy. Historical completion and current effective
  inclusion are separate facts; changed edges do not erase an acknowledged PR.
- **Application:** command/query orchestration through ports, authorization,
  transaction scopes, revision checks and publication order. Storage failures do
  not authorize a ref rewind, guessed source binding or loss of retry work.
- **Adapters:** PostgreSQL/FS, Git and provider file operations, HTTP/MCP/hook
  protocol mapping. Composition roots choose concrete adapters. Public adapters
  consume application contracts rather than reconstructing domain decisions.
- **Web:** server query results plus local selection, folding, scroll, hover and
  graph coordinates. Hiding a node is not deleting an event or revoking merge
  completion. `EventStream` renders transcript rows independently of the page
  that fetches them; formatting helpers do not depend on pages.

`docs/CONTEXT_HISTORY.md` defines the history, memory, effective-state and active
session notice contracts. These boundaries are incremental safeguards, not a
claim that directory names alone prove domain correctness.

## Production store requirements

`cxtd` validates the required `ProductionStore` capability set before migrations,
workers or HTTP startup when PostgreSQL is configured or required. One concrete
store must provide transactions, workspace access locks, revisions, durable jobs,
lease fencing, outboxes, shared runtime/authentication state, immutable document
publication, indexed reads and Git evidence. Missing capability is a startup
error. PostgreSQL also has a build-time interface assertion.

FS remains an explicit development/test adapter and is not accepted as the cloud
transaction boundary. Interface conformance is necessary but does not prove
ACID: real PostgreSQL tests cover rollback, racing commands, authorization changes,
revisions, leases and duplicate delivery against shared state.

## CLI native capture and local retry work

`SessionCapture` is injected into save/stash use cases. Its adapter owns native
transcript access, masking and disposable incremental projection checkpoints.
The full native prefix and masking policy are verified before reusing projected
chunks; the content-addressed document is durable before a checkpoint is saved.
The application publishes snapshots and refs only after successful capture.

Native session materializers own their capture-baseline ledger record. Both
Claude and Codex recovery output stays excluded from recapture until actual
provider conversation grows; application code does not manipulate provider files.
Session identifier validation is a domain value rule.

`SyncOutbox` is injected into save/sync. Its adapter owns queue JSON, legacy-format
reads, atomic file replacement and process locks. Application code owns ordered
CAS graft retries, conflict handling and queue-before-snapshot order. No network
request runs inside a queue lock. Promotion acknowledgements compare the exact
sent value under lock, preserving concurrent replacements and unrelated writes.
Corrupt queue bytes are retained and rejected. An output queue is not a distributed
transaction: failed remote delivery is retried idempotently rather than described
as exactly-once transport.

## Continuous checks

- Backend `internal/architecture` parses non-test Go imports in both modules:
  domain cannot import ports/application/adapters, ports cannot import application
  or adapters, application cannot import adapters, and internal packages cannot
  cross the backend/CLI module boundary. Integration tests may assemble adapters.
- Web `npm run check:architecture` uses the TypeScript parser and module resolver,
  including aliases and barrel exports, to reject runtime dependency cycles.
  Explicit type-only edges are exempt; mixed and side-effect imports are checked.
- API contracts, real browser E2E and provider/sync E2E remain required. Dependency
  checks complement those regressions rather than replacing behavior verification.
