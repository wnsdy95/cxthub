# Reliability and Enterprise completion

Baseline: `0f9ffcf`, 2026-09-28. Tracking: #144 and #288.
Cloud provisioning/deployment and paid billing activation are excluded. The
existing repository, organization and Enterprise authority boundaries remain.

## Delivery order and acceptance

Current performance priority (2026-10-19):
[architecture design](INCREMENTAL_READ_PUBLICATION_DESIGN.md) has been approved
for staged implementation. D1 admission and D2 pinned-header/readiness,
durable missing-index work, source-version-fenced compact generations and resumable
exact inherited boundaries are locally implemented and validated. The
mode-specific visible index, explicit HTTP unit/fold pagination and separate Web
validator/renderer are now implemented locally. Focused browser and actual
PostgreSQL-to-Web wire checks pass; mixed-load and release gates remain separate.
The first D3 source-journal boundary is also implemented and locally validated:
missing repository-view metadata shares the existing transactional catalog
clock without changing public CLI kinds. The next local slice implements
opt-in durable pending generations, retention-protected workers and an explicit
catch-up/readiness contract. Cheap revision/SSE metadata now distinguishes source
changes and projection readiness even when legacy counters do not change;
notification and body-read validation remain separate. A subsequent private
graph-source boundary captures Git/PR proofs and fences process caches against
source-only changes and catalog reset; it is not a durable graph cutover.
Frozen graph reconstruction now uses only completed source images, with no live
proof fallback. The next graph publisher persists page progress, an independent
retention pin and an atomic complete result through the shared fenced consumer
engine. A subsequent default-off Web reader consumes a rules-2 graph/metadata
bundle, exposes source/applied checkpoints, retains coherent lagged results and
reauthorizes after admission. Full/live/default/historical graph parity now has
local PostgreSQL coverage. Bounded corrupt-generation repair now queues after
owned reads, coalesces concurrent reports, survives restart, preserves terminal
failures and quarantines repeated corruption without rewriting source data.
Mounted browser checks and complete frontend/backend test runners pass locally.
Bounded persisted shadow comparisons and an exact-generation experimental reader
gate are locally verified, including actual PostgreSQL restart/mismatch/input
limits and independent review fixes. A separately retained qualified serving
head now keeps the last matching metadata bundle readable while newer candidates
are published or fail comparison. Atomic promotion, monotonic serving fences,
candidate/serving repair isolation and legacy-receipt bootstrap have local
PostgreSQL race coverage. Initial qualification, continuous-write freshness and
recovery after losing the only serving copy still require catch-up verification.
Independent D3h review identified further retry-classification, unavailable-head
status, promotion byte-bound and quarantine races. Their fixes, deterministic
interleaving tests and re-review are complete; final targeted PostgreSQL and unit
races pass locally. This does not close continuous-write catch-up or release gates.
D3i closes reproduced current-query continuation gaps: effective-memory proof
reuse/cursors now include the read-only committed source checkpoint, and context
segments bind selected delivery provenance across Git-origin changes. Immutable
memory/component ownership and bytes remain checked before any proof-cache hit;
the graph generation is not a memory-ready or access certificate. Direct AI
queries still do not consume lagged Web bundles. Actual HTTP/MCP-to-PostgreSQL
continuation parity, inline/chunked ownership/hash checks, focused race runs,
full normal backend tests, PostgreSQL vet, builds and independent review pass
locally. D3j adds shared bulk admission for all six public memory reads and
transport-owned scopes through response assembly. It avoids waiting in borrowed
transactions, revalidates transport authority after owned queue waits and expires
nested authorization at scope exit. Core and disposable PostgreSQL admission,
cross-replica revocation, HTTP/MCP serialization/cancellation, existing continuation
contracts, independent review and the final complete backend runner pass locally.
The incremental plan now has a compact D1–D5 checkpoint map; its remaining gates
are not closed by this admission work.
D3k adds an independent transactional memory-metadata generation for legacy
metadata, PR/Git pins and root/component ownership. Current effective-memory
continuation binds it with the graph checkpoint from the same read snapshot;
semantic delivery identity remains unchanged. Focused unit/actual PostgreSQL
race checks, the full normal backend suite, PostgreSQL vet, builds and independent
review pass. Shared-byte fan-out was rejected:
equal metadata generations do not prove current bytes or retention, and
ownership/hash checks still precede proof reuse. Captured object dependencies,
retention and durable memory serving remain open.
D3l separates read-only memory selection inputs from general writer adapters.
An internal coherent capture records demanded object bytes and observed absence,
then detached projection uses the same selection/assessment/pagination rules
without live store fallback. PostgreSQL capture bounds object decompression,
logical repeated components and exact-pin metadata. This is an input/replay
boundary, not persisted staging, retention or a current-serving cutover.
Actual PostgreSQL mutation/parity and store-limit tests, focused race runs and
independent review pass locally. Final review also closes a cumulative stored-
payload gap: valid compressed padding across components can no longer bypass
the per-digest transfer budget. This is not production latency/RSS qualification.
The final complete normal backend suite, PostgreSQL vet and API/MCP builds also
pass. The disposable PostgreSQL cluster is stopped; no binary is installed and
no migration or deployment is applied by this slice.
D3m adds restartable, content-addressed frozen memory inputs with atomic
PostgreSQL persistence, shared blocks, bounded expiry/pruning and strict restore.
Replay survives process restart and original-input mutation without treating old
bytes as current access authority. Empty chunks cannot masquerade as observed
absence. Focused race/actual-PG regressions, the complete normal backend suite,
vet/builds and independent review pass locally. This is not incremental
current-memory preparation or qualified output serving: those contracts, original
object lifetime and the live endpoint cutover remain open. No deployment occurs.
D3n adds a selection-scoped persisted result and internal range reader. Strict
preparation rejects exhausted item/receipt/branch traversal budgets instead of
sealing incomplete review output. Missing evidence is still an explicit review
state. Source-capture expiry bounds result lifetime; historical result bytes do
not authorize current reads. D3o replaces repeated header/index checks and
decoding with a scoped range operation, but incremental preparation,
qualification and current authority/source checks remain open. No API/MCP
cutover is claimed by this persistence work.
Final focused race, actual PostgreSQL, vet/build and complete normal backend
checks pass. Earlier package/fixture timeout attempts are retained separately;
unchanged rechecks pass without extending guards. The owned test cluster is
stopped, and production is untouched.
D3o's real PostgreSQL two-chunk range uses five SELECTs/five rows at both 640 and
6,400 result items; the latter point-read baseline uses 14 SELECTs/311 rows.
Remaining header work is O(index), not constant. Scope expiry/cancellation,
selected corruption, callback ownership, maximum pages and audit separation
pass targeted race/actual-PG checks; vet and builds pass. Independent reviewer
capacity was unavailable, so no independent sign-off is claimed for D3o.
D3p adds verified selected-object witnesses using the existing blob write counter
and new delete/reinsert incarnations. Fresh validation rejects stale borrowed
transactions and checks current grants without rereading unchanged payloads.
A shared domain budget includes roots and components across the whole capture.
Actual PostgreSQL five-object checks use one SELECT/380 received bytes despite
10x unrelated-object growth. Upgrade/rollback, ABA, interleaved writes, revoked
grants, absent dependencies and document fences pass focused race regressions;
broader memory races, compile/vet/build and independent source re-review pass.
This is not durable qualification or current endpoint serving; retained source
lifetimes, incremental preparation, migration lock-time and rollout gates remain.
D3q adds worker-only preparation/publication and immutable qualifications binding
capture/result identity, source versions and selected object witnesses. Fresh
fenced publication handles concurrent writers without lock cycles; a new scoped
reader rechecks current sources before selected result chunks. Restart, rollback,
expiry, revocation, root-v1 compatibility and 10x unrelated-object cost checks
pass locally, along with focused/broader race tests, compile/vet/build and
independent source review. This is an internal boundary, not a transport cutover:
ordinary callers must provide an explicit snapshot-safe authorizer. Existing
HTTP/MCP login/migration/break-glass callbacks perform writes and cannot be reused
there. D3r separates those operations before current endpoint integration.
Incremental preparation, durable worker leases/retries and rollout remain open.
D4a starts publication-cost work without depending on or claiming D3 serving
completion. Streaming root final verification batches retained current bytes;
the previous materialized acceptance contract is unchanged. Repeated evicted
chunks reload alone rather than amplifying transfer with an old window. Legacy
publication, safe preparation/short-commit separation and independent derivation
workers remain open, as do the full mixed-load and rollout gates.
Rollout qualification, effective-memory dependencies, complete MCP cutover and
production validation remain open; worker/shadow/read flags are independent.
The remaining design, release and production gates are still open. The
[incremental publication and bounded reads](INCREMENTAL_READ_PUBLICATION_PLAN.md)
specifies cold-read admission, conversation range indexes, durable graph/pending
projections and shorter publication transactions, in that order. It preserves
the automatic-association invariants below; its unchecked gates are not completed
by earlier local range-read or deployment checks.

Current priority (2026-10-10): [automatic context association](AUTOMATIC_CONTEXT_LINKING.md)
tracks #494 and the related #492 delivery gap. GitHub changes, ordinary local
fetch/pull and server reconciliation must converge without a customer-operated
repair procedure. This supersedes a CLI pre-merge check as the proposed fix;
original capture provenance and exact-publication validation stay immutable.

2026-10-11 checkpoint: semantic historical contributions and publication-pinned
memory now have a separate immutable ledger; verified late work does not replace
the current main ref. PostgreSQL contribution/pin/job rollback and concurrency
tests and common context/graph query regressions pass. Final backend test/vet,
focused PostgreSQL race checks, frontend unit/build checks and 63 UI regression
browser tests pass (two full-stack-only tests skipped). Installed-service checks,
public contribution diagnostics, #492 integration and real #488 recovery remain
open; changes are uncommitted, with no deployment or manual live-data repair.

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
- Prerequisite #320 shipped in #321; independent API/MCP processes use the shared
  PostgreSQL grant state and the local services run the merged binaries.
- Credential-assurance work #322 adds explicit per-Enterprise CLI/MCP approvals,
  exact browser/credential binding, a revisioned 1–24 hour lifetime (default 8),
  and audit in the identity transaction. Protocol-envelope validity and recorded
  session life are distinct; SAML IdP session limits still apply. Refresh preserves
  original authentication and cannot renew approval. API/UI expose server-computed
  state and non-bearer selectors. Mandatory policy enforcement and recoverable-owner
  activation remain the next security boundary; this feature alone does not
  restrict repository reads/writes or declare SSO/MFA enforced.

- Identity prerequisite #328 adds a shared credential assessment and owner recovery
  preparation/confirmation/redemption. Browser, CLI and MCP evidence keep their
  separate identities and lifetimes. Recovery is one-use, browser-bound, audited
  and invalidated on ownership loss, with no repository or SSO grant. This is not
  mandatory SSO/MFA enforcement or working policy repair: the next slice must
  apply assessment to protected reads/writes and wire narrowly scoped identity
  repair commands with safe last-owner activation. SCIM, retention, external
  security-audit delivery and live customer-provider acceptance remain open.
