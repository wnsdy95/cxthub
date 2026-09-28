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
