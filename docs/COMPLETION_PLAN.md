# Reliability and Enterprise completion

Baseline: `0f9ffcf`, 2026-09-28. Tracking: #144 and #288.
Cloud provisioning/deployment and paid billing activation are excluded. The
existing repository, organization and Enterprise authority boundaries remain.

## Delivery order and acceptance

1. **Retained capture recovery.** Inspect attempts across worktrees without
   changing them. Distinguish exact replay, independently completed publication
   at the same code position, and an unprovable historical gap. Persist recovery
   decisions separately with immutable evidence and concurrency checks. Never
   substitute a live transcript or a newer worktree position for an old capture.
   Validate the three retained development cases through supported commands.
2. **Synchronization cost.** Measure selection, transfer, validation and indexing
   independently. Separate foreground prerequisites from retained historical
   backfill without losing either. Reuse verified chunks/index components where
   their identity proves equivalence; preserve whole-document identity checks.
   Persist deferred work, bound retries and demonstrate progress under capture,
   cancellation, restart and concurrent publication.
3. **PostgreSQL development cutover.** Back up and freeze current FS writers at
   cutover, import a fresh verified copy into an empty local database, compare
   identities/authority/history/memory and rehearse restore. Run independent API
   and MCP against that database. Preserve the original FS backup and rollback
   route; no external resources or production transfer.
4. **Progress and large graphs.** Expose transfer/verification/publication phases,
   actionable recovery and retained gaps consistently. Render only the visible
   graph rows while retaining the full server-provided graph evidence, lane
   coordinates, sticky labels, selection and keyboard access. Verify scrolling,
   folding, live updates and long labels with browser regressions.
5. **Enterprise identity and security.** Add interoperable SAML/OIDC connection
   and verified-domain identity binding, SCIM with source-aware deprovisioning,
   enforced MFA/IP/session policies and last-owner recovery. Add auditable
   retention/deletion/restore controls and security audit delivery. Configuration
   alone must never claim enforcement. Existing users, manual grants and unrelated
   organizations must survive identity changes. Local protocol/IdP fixtures are
   required; actual customer/provider credentials remain a separate verification.

Each logical change gets focused failure-path tests, affected full checks, a
signed PR and green CI. Record measured limitations rather than treating local
tests as cloud certification. Preserve user data and existing Git/provider hooks.

## Design review constraints

- Capture recovery from the current transcript is rejected: it falsely assigns
  later conversations to an earlier commit. Immutable evidence plus distinct gap
  and replacement states is the selected approach. Repeated warnings alone do
  not provide an operator recovery workflow.
- Incremental work must not weaken canonical hashes or classify retained history
  as disposable. A queue acknowledgement must imply durable retriable work.
- Graph virtualization is a presentation optimization. Business inclusion and
  publication facts stay in the server query contract.
- Enterprise membership is not private repository access. Identity federation
  cannot merge accounts merely because emails match, or silently remove manual
  grants. A tenant must not lock out its last recoverable owner.
- Retention defaults preserve data. Policy controls and deletion mechanisms are
  tested with synthetic data; enabling deletion for live data requires a concrete
  policy selected by the administrator. Billing counters never authorize deletion.

## Status

- Capture attempt investigation: two failed attempts have a later complete pass
  and publication at the same Git/worktree/branch position; one has no successful
  capture result. The original journals remain unchanged during investigation.
- Enterprise IdP: use local protocol fixtures unless a customer IdP is supplied.
- Capture recovery CLI and failure-path tests implemented. Two retained failures
  were resolved through verified replacement evidence; all three original attempt
  files remained byte-for-byte unchanged. One unprovable gap remains unresolved
  pending an explicit operator acknowledgment. CLI suite/vet/race and real
  Git-hook sync E2E passed. PR #289 merged after all nine CI checks passed.
- Synchronization phase: #290 tracks remaining latency. History verification
  now reuses authenticated storage receipts and removes per-operation duplicate
  validation; synthetic cold/warm measurements and corruption tests are recorded
  in PULL_VALIDATION_PROGRESS.md. PR #291 merged with nine green CI checks and
  the exact main-source CLI was installed locally. PostgreSQL indexing now reuses
  current-version event search rows by verified event hash, retaining them against
  concurrent deletion. FS filters stream trigrams without a transcript-sized
  intermediate array. PR #292 merged with nine green CI checks. Foreground/backfill
  separation is implemented with a durable versioned queue, capture-retention
  pins, bounded retries and read-only doctor diagnostics. CLI suite/vet, focused
  race tests and full Git-hook E2E passed. PR #295 merged after nine green CI
  checks; its exact main-source CLI was installed. Live backlog completion has
  not yet been established. Delayed upstream-based branch reclassification was
  fixed in PR #298 (nine green checks): the prepared journal freezes the binding,
  and the retained live birth replayed successfully through the CLI. Missing
  historical evidence remains queued rather than borrowing current upstreams.
  Fast queue-only status and explicit push/pull progress passed the CLI suite,
  vet, focused race checks and real Git-hook E2E. Queue-only inspection completed
  in 1.18 seconds with 85 retained jobs in the local development repository;
  it deliberately does not claim server acknowledgment or full integrity.
  PR #299 merged after nine green CI checks.
  Uploaded-chunk finalization now avoids discarded whole-document compression
  and repeated compression/upload of existing chunks while retaining exact byte
  validation and transaction locks. Local PostgreSQL persistence benchmarks and
  corruption/concurrent insertion/deletion regressions passed; see
  PULL_VALIDATION_PROGRESS.md. This does not yet resolve queue wait or establish
  completion of the live backlog.
- PostgreSQL cutover: #294. The independent local service launcher, private
  configuration, credential separation and startup rollback have synthetic tests.
  PR #296 merged, followed by the actual local cutover. The frozen FS backup
  matched all 25,036 files (8,963,611,276 bytes); independent dump/restore matched
  all 73 PostgreSQL tables and 27,701 rows before workers started. API and MCP
  run as separate persistent loopback services. Authenticated API and MCP reads,
  OAuth PKCE and revocation passed; the server audit reported Missing 0.
  Original FS data/binary/config and verified backups are retained privately.
  The old FS launch agent is disabled to prevent an accidental dual start.
  Issue #294 is complete; this is local acceptance, not cloud load certification.
- Large graph presentation: #297. Full server evidence and lane layout are
  preserved while visible rows, overscan and focused/dragged rows bound the DOM.
  Unit geometry checks, build/i18n/architecture and all 79 browser regressions
  passed with independent API/MCP and a disposable PostgreSQL database. Focused
  replacement coverage also passed. PR #300 merged after nine green CI checks.
- Enterprise domain verification now has a PostgreSQL transaction boundary,
  owner-managed DNS challenges, audited release and an administration UI.
  Race tests cover competing tenants, owner revocation, rotation and release
  during DNS lookup. This grants no identity or repository authority. Remaining
  federation, provisioning and enforcement slices are in ENTERPRISE_IDENTITY.md;
  the overall Enterprise identity/security plan is not complete.

- OIDC browser verification adds encrypted provider credentials, PKCE/nonce,
  durable single-use attempts and explicit issuer/subject binding to the initiating
  browser session. Existing membership and repository access do not change.
  Mandatory SSO, SAML, CLI/MCP assurance renewal and security policies are not yet
  enabled; detailed boundaries are recorded in ENTERPRISE_IDENTITY.md.

- OIDC PR #304 merged after nine green CI checks. The first sync E2E run failed
  during a second repair and passed on rerun; investigation exposed the separate
  two-observation repair gap in #305. Repair now uses the refs from its verified
  pull. Cumulative document validation reuses bounded exact-event proofs while
  preserving full hashes, CIR-version checks and current chunk ownership. Cold
  and warm costs and limits are recorded in PULL_VALIDATION_PROGRESS.md. Neither
  change claims that the entire live synchronization backlog is drained.

- Synchronization proof/repair changes merged in #306 after nine green CI checks.
  Main-source `e2946ba` CLI, API and MCP binaries are installed locally and both
  independent services are healthy. Live retained-history publication is being
  measured; upload progress is not a substitute for ref acknowledgment.
- SAML browser verification (#307) is implemented with pinned metadata, signed
  requests/assertions, durable replay records and a browser-bound two-step
  callback. Local PostgreSQL and signed protocol fixtures are under validation.
  It is not mandatory SSO, SCIM, policy enforcement or customer IdP acceptance.

- SAML PR #308 merged after nine green CI checks. The exact main-source API and
  MCP binaries were installed and both PostgreSQL-backed services passed health
  checks. Browser verification does not enable mandatory SSO, CLI/MCP assurance,
  MFA/IP/session enforcement or customer IdP activation.
- Sync follow-up #309 removes full transcript reads for already-protected pending
  history and uses verified canonical prefixes for eligible supersession checks.
  Synthetic comparison costs and the observed live lock limitation are recorded
  in PULL_VALIDATION_PROGRESS.md. Backlog completion remains a separate live check.

- Pending-maintenance PR #310 merged with nine green CI checks; the exact
  main-source API/MCP binaries were installed and passed health checks. Follow-up
  #309 now shares derived event-location blocks while preserving v2 reads and
  atomic publication. Synthetic indexing measurements are recorded separately
  from full synchronization progress in PULL_VALIDATION_PROGRESS.md.

- Shared event-location blocks merged in #311 after nine green CI checks. Exact
  main-source `f144228` API/MCP binaries are installed and healthy. Live retained
  history upload is under observation; completion is not inferred from indexing
  benchmarks. Enterprise encryption-key lifecycle #312 adds staged read/write
  keys and a transactional, auditable operator rewrap workflow. Mandatory policy,
  credential assurance, SCIM, retention and security delivery remain outstanding.

- Encryption-key lifecycle #312 merged in #313 after nine green checks. Exact
  main-source `c5f24a4` API/MCP/operator binaries are installed; API/MCP are healthy.
  No live IdP or key configuration was activated. The actual synchronization run
  completed its document/snapshot stages but encountered concurrent remote progress
  at the final ref batch. Ordinary pull preserved local-ahead branch work. #309
  now also bounds CLI snapshot publication per transaction and reports distinct
  memory/history progress; live backlog acceptance is still pending.

- Bounded snapshot publication merged in #314 after nine green CI checks. Exact
  main-source `7af904e` CLI is installed. A live run completed 12 documents and
  all snapshot/memory/history stages; final refs encountered concurrent remote
  progress. Pull preserved 14 local-ahead branches, verified against both graphs.
  A subsequent ordinary push completed 16 snapshots and all 2,534 refs; all 14
  previously retained branch tips match the server. The historical upload queue
  is empty. No force/append or manual data repair was used. Full sync still
  selects superseded, unreferenced hook captures retained locally; recurring
  capture traffic/performance remains in #290 rather than claiming cloud readiness.
- SAML signing lifecycle #315 adds owner-confirmed preparation/activation,
  current-revision browser verification before retirement, cancellation and valid
  previous-key rollback. IdP trust repair preserves both keys and resets verification.
  Expired SP certificates do not prevent publishing recovery metadata. Alternate
  keys participate in operator encryption rewrap. Local signed fixture and race
  checks passed, along with 82 browser regressions, focused UI recheck and full
  backend/PostgreSQL test/vet. No customer IdP or mandatory policy is enabled.
- SAML signing lifecycle merged in #316 after nine green CI checks. Exact
  main-source `472a99e` API/MCP/operator binaries are installed and healthy;
  migration 0068 is applied. No live identity configuration was changed.
- Sync conflict diagnostics now retain the server's rejected ref names and typed
  causes across bounded batches and legacy single-ref requests. A later terminal
  failure keeps earlier diagnostics without becoming a retryable conflict. The
  next #290 storage-cost investigation must distinguish inherited pinned memory
  from independently authored attachments; memory-bearing captures are not
  disposable merely because their conversation is a prefix of a newer capture.
- Typed ref diagnostics merged in #318 after nine green CI checks. The exact
  main-source `dd6cf9b` CLI is installed. Follow-up #317 reduces repeated memory
  request bodies using exact-hash reuse of an immutable repository-owned base.
  Existing memory identity, projection, source provenance and retention stay
  unchanged. Local synthetic transfer measurements and limits are recorded in
  PULL_VALIDATION_PROGRESS.md; this does not close the remaining #290 latency work.
- Memory-body reuse merged in #319 with nine green CI checks. Exact main-source
  `4dc2e63` CLI/API/MCP/operator binaries are installed. All three local services
  respond successfully and the context audit reports Missing 0. Synthetic transfer
  savings do not certify end-to-end cloud latency or close #290.
- Enterprise prerequisite #320 fixes MCP refresh relationships before credential
  assurance is added: one grant per consent, stable identity through rotation,
  committed replay revocation and audit, separate legacy-token handling, and no
  cross-device SSO inference. This does not enable mandatory SSO/MFA, SCIM,
  retention or external security-audit delivery. Those remain implementation work.
  Local full backend/PostgreSQL test and vet, focused FS/PG race repetitions,
  OAuth wire regressions, OpenAPI drift, public-tree checks and real Git-hook
  PostgreSQL E2E passed. The pre-change replay regression failed as expected;
  the fixed version blocks the entire affected grant while retaining peers.
