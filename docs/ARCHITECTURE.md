# Architecture and enforced boundaries

CXTHub stores shared repository facts in the cloud backend. The CLI's `.cxt`
store is a local replica, recovery outbox and worktree selection cache. It is not
the authoritative database for Web or remote MCP queries.

## Server deployment boundary

Production uses Vercel for the frontend, independent Render `cxtd` API and
`cxt-mcp` services, and shared PostgreSQL. `internal/serverruntime` initializes
common store/authentication/lifecycle dependencies without starting workers.
`internal/mcpserver` composes only remote MCP/OAuth and query ports. API keeps
all GitHub, ingestion, promotion, notification and maintenance workers. A
transitive import test prevents recombining these executable dependencies.
Both services reuse application rules and PostgreSQL transaction contracts;
separation does not introduce a second authorization model or copied data.
See [deployment](../deploy/RENDER.md) for routing and rollout order.

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
store must provide transactions, repository access locks, revisions, durable jobs,
lease fencing, outboxes, shared runtime/authentication state, immutable document
publication, indexed reads and Git evidence. Missing capability is a startup
error. PostgreSQL also has a build-time interface assertion.

FS remains an explicit development/test adapter and is not accepted as the cloud
transaction boundary. Interface conformance is necessary but does not prove
ACID: real PostgreSQL tests cover rollback, racing commands, authorization changes,
revisions, leases and duplicate delivery against shared state.

Context ref validation checks the repository protocol before deciding whether
branch history is required. In protocol 1, branch writes still require durable
identity evidence and validate it inside the graph transaction. Head, session
and tag refs do not use that branch projection, so their protocol check does not
load the full history or look up a branch identity. Unsupported protocol versions
remain errors for every ref kind. This narrows only the protocol validation
read set; authorization, journal recovery, object checks, compare-and-swap and
publication still run in their existing command and storage boundaries.
FS ref writes reject unreadable existing pointers instead of treating them as
absent. After journal recovery, protocol-1 symbolic HEAD writes recheck their
branch target under the repository lock; recovery cannot leave HEAD attached to
a branch it just removed. Legacy unborn HEAD bootstrap remains supported.

Document finalization separates pure preparation from publication. The worker
verifies current owned v2 chunks and retains an immutable segmented document;
publication reuses those chunk boundaries instead of assembling and re-chunking
the cumulative transcript. Its legacy whole-document identity still requires
streaming all canonical bytes through the hash. V1 remains a compatibility path.
The worker renews its durable lease while the PostgreSQL adapter prepares chunk
references and read-index blocks from that verified document. Preparation acquires
no database transaction and grants no ownership. A private prepared-publication
handle then rechecks the current job version, lease and document hash inside the
repository transaction. Chunk retention/integrity checks, selective search-row
construction, quota accounting, ownership and the completion receipt still
commit together. The lease is not renewed concurrently with the locked job row.
An expired or replaced worker cannot publish its prepared result. This reduces
CPU work under the repository lock; it does not remove database contention or
make document publication cost-independent of document size.

[Conversation chunks and memory versions](CONVERSATION_MEMORY_STORAGE.md)
defines why memory recompression publishes a separate version without replacing
conversation chunks. A manifest-root protocol that removes full-stream hashing
is a separate compatibility change, not implied by this allocation improvement.

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

## Local CLI reads and transport cleanup

Local CLI history reads compare fresh complete snapshot/ref observations before
projecting ancestry. `SnapshotCatalogReader` is an optional read port that omits
the presentation sort; the application still canonicalizes and validates every
record, including unreachable metadata. Adapters without it retain the ordinary
`ListSnapshots` path. `FileStore` shares validation between the two paths and
honors cancellation while scanning, without returning a partial catalog. Neither
path caches mutable metadata, takes a mutation lock or repairs local state.
Status retains its complete working-state fence, and diff revalidates after
reading documents. Unchanged log/status/diff therefore still enumerate metadata
2/4/6 times; eliminating a redundant sort is not a disk-scan reduction.

The REST client drains unused successful acknowledgments to support HTTP/1
connection reuse, bounded by 32 KiB and a 100 ms cleanup timer started after
successful headers. The ordinary request deadline remains unchanged. A failed
cleanup closes the response but does not turn an acknowledged write into a
failure or retry. Responses with a consumed application payload retain their
existing decoding/error contract.

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

## Server-owned graph reads

`domain.ProjectGraphState` decides publication tiers, branch identity, archived
membership, retained-progress groups, On Hold clusters and supported birth/merge
operations. It shares branch bindings and completion semantics with the existing
context query used by REST and MCP. MCP does not need SVG layout metadata.

`GetRepositoryView`, `GetPendingView` and `QueryGraphState` load metadata in one
repository read transaction. PostgreSQL pins one MVCC generation across metadata,
revision and projection. Pending responses carry the same classification contract
but only current capture metadata; they omit branch memberships owned by the full
view. Missing new inputs trigger one full browser refresh, not local inference.

There is no cross-request graph cache: staging metadata can change before its
publication revision advances. A revision-only cache could serve stale inputs on
one replica. Per-read ancestry bitsets have an 8 MiB / 4,096-entry cache limit;
exhaustion changes cost, never correctness. Documents are not loaded for graph
classification.

The HTTP graph adapter uses `indexed-v1`: repeated ID sets reference one response
local dictionary. These indices are neither persisted identities nor business
state. Web validates and decodes the transport, resolves IDs, and then handles
folding, selection and geometry. `/view`, `/pending-view` and `/graph-state` support
gzip inside their viewer authorization guard; SSE is not compressed by this
adapter. Deploy the backend contract before the corresponding frontend. A missing
or unsupported contract fails explicitly rather than silently restoring old
frontend policy rules.

Browser fixtures invoke the actual Go domain projection and wire encoder. The
same tests cover live capture changes with PR history, superseded grafts,
renames/name reuse, read-only position selection and invalid-response recovery.
A rejected refresh preserves the last coherent conversation; an invalid first
read shows an error and does not draw a guessed graph.

Session archiving uses a separate repository-wide `SessionArchiveStore` port.
The application owns authorization and transactions; a domain projection groups
provider/session snapshots and resolves evidence-backed provenance. The Web
receives explicit archived snapshot IDs, then filters visibility only after the
complete branch graph projection. Archiving never changes publication tiers,
branch refs, PR completion, memory applicability or MCP/CLI context selection.
See [session archives](LIVE_CAPTURE.md#session-archives).
