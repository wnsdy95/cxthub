# Automatic context association

Tracking: #494 (rewritten PR source), #492 (generic merge delivery).
Approved product contract: 2026-10-10. This is an implementation plan, not a
claim that the whole automatic path is already available.

## Customer contract

After connecting a repository and enabling capture, customers use ordinary Git
and GitHub workflows. They do not need a pre-merge CXT command, an approval
dialog, database access, or manual graph repair to associate existing context
with their merged work. Web and MCP consume the same server-owned result.

Automatic does not mean inventing missing conversations or claiming code
equivalence from a branch name. Missing provider access or unavailable original
evidence remains a truthful, monitored service state. The service retries and
reconciles automatically; it does not mark the operation complete or fabricate a
capture to remove an alert. Restoring deliberately revoked external access is
not something the service may bypass.

## Confirmed failure

PR #488 had an accepted source publication at an earlier Git head. Two
GitHub-side rebases changed the head without executing local post-rewrite hooks.
The merged PR names the last head, for which no exact publication was found.
Current PR promotion requires an exact source publication and correctly refuses
to relabel the old capture. Repeating that lookup cannot create missing proof.

Two observation gaps are confirmed in the implementation:

- GitHub delivery processing ignores non-merged PR actions. The delivery is
  subsequently acknowledged and its raw body cleared, without retaining a
  domain observation for a synchronize event.
- The local reference-transaction path handles local heads and stash, not
  remote-tracking ref changes caused by fetch. Pull discovers merged PRs, but
  this alone does not reconstruct an earlier source association after rewrite.

The separate #492 failure loses the original generic merge range after a failed
attempt. Its durable queue work must retain that range without allowing delayed
older work to replace a newer selected tip.

## Three inputs, one authority

1. Authenticated GitHub webhooks provide timely PR/head/ref observations.
2. Ordinary local fetch/pull records observed remote ref changes durably and
   submits retryable discovery hints without moving the current worktree context.
3. Server reconciliation checks provider PR/ref history to recover missed
   deliveries independently of an online client.

All three inputs converge on the same application use case. Local reports are
discovery hints, not authority to assert a GitHub PR merge, rewrite equivalence,
or arbitrary repository association. Never trust client-supplied remote URLs in
place of the repository's configured provider binding.

Fetch observes the old and new local remote-tracking ref values, not every
intermediate remote rewrite. GitHub/provider history supplements this evidence.
Refspecs, multiple remotes, shallow clones, pruning, branch-name reuse and
out-of-order arrivals must not be interpreted as complete remote history.

## Separate business facts

- **Original capture/publication:** immutable conversation chunks and the actual
  code/worktree/branch position at which they were captured and published.
- **PR participation:** provider-verified evidence that an original publication
  belonged to a historical head of this specific PR and repository identity.
- **Current code applicability:** whether a memory claim's declared code scope
  still applies at the selected code position.
- **Integration completion:** the server durably linked the verified source
  records into the target history, independently of present graph folding.

PR participation does not prove identical code before and after rebase. A
replaced implementation can remain relevant historical conversation while its
code-specific memory is no longer applicable. No synthetic publish event may
pretend an old capture was produced at a new SHA. Exact existing publications
retain their current validation contract.

## Integrity and transaction boundary

GitHub, customer Git repositories and PostgreSQL cannot share one ACID
transaction. Observation/delivery is asynchronous and retryable. Within
PostgreSQL, each accepted transition must be ACID:

- **Atomicity:** association evidence, context integration, repository revision,
  completion receipt and required outbound work commit together, or not at all.
  Original chunks can already exist as validated, unreferenced staging objects.
- **Consistency:** verify repository/installation/branch identity, original
  publication and object hashes; preserve natural parents and reject cycles.
- **Isolation:** use the existing repository transaction and lease fences;
  recheck binding generation, access policy and expected state after external
  reads. Expired workers and duplicate clients cannot apply conflicting results.
- **Durability:** acknowledge accepted work only after durable storage. Restart
  and lost responses resume the same immutable operation, not a newer range.

No provider HTTP calls inside a database write transaction. Fetch bounded proof
outside it, then validate its scope and current policy under the transaction.
Exactly-once effect uses stable operation identities and transactional receipt
checks; external event delivery itself is not exactly once.

Late historical integrations must be placed according to verified Git
relationships without making an older source the new tip or dropping later
contributions. Parallel work has a deterministic tie-breaker, not fabricated
ancestry. Revert/reset/current selection remain separate from historical joins.

## Implementation sequence

| Phase | Deliverable and acceptance | Status |
| --- | --- | --- |
| 1 | Persist normalized PR head changes before webhook acknowledgement; preserve source identity, before/after, action and provider delivery evidence; duplicate/reordered delivery does not move context refs | Implemented; synthetic and PostgreSQL rollback tests pass; not installed |
| 2 | Durable remote-ref observations on ordinary fetch/pull, automatic discovery handoff/replay, no provider restart or worktree-position movement | Implemented with existing-observer recovery and focused crash/expiry fixtures; serial CLI suite and focused race checks passed; no always-on client guarantee |
| 3 | Provider-history reconciliation and an explicit server-verified PR participation proof; automatic resolution of original accepted publications across rewritten heads | Server consumer, durable checkpoints and participation evidence implemented; phase 4 now consumes accepted proof |
| 4 | Integrate proof, context order and completion transactionally; preserve later work and original capture provenance; apply existing scoped memory validation | Implemented and locally validated: immutable contributions/pins, ordering-safe completion, shared query projection and capped automatic retries |
| 5 | Web/MCP query parity, crash/concurrency/authorization regressions, independent review and installed-binary verification | PR query parity, historical delivery acknowledgement and PostgreSQL conditional-append fences validated; generic application, late memory supplements, ordered cross-page admission, customer diagnostics, indexed worker evidence and request-local query indexes have local coverage below. Installed-service verification and real #488 recovery remain pending |

The previous CLI-only publication checker remains an unfinished diagnostic
experiment in its separate worktree. It is not a customer requirement and does
not close #494. The #492 queue patch is now integrated in this development
worktree. Its reviewed identity/scheduling/late-source safeguards remain intact;
combined validation and the automatic recovery additions below are in progress.

### Current implementation boundary

The local detector records committed remote-tracking ref changes only in an
already connected repository. Standard origin mapping can coexist with disjoint
secondary remotes. Replays recheck the essential repository/destination binding
without treating unrelated config formatting as a new identity. Ambiguous
refspecs, unavailable commits and shallow history retain unresolved evidence.
There is no automatic unshallow, invented source capture, context ref movement,
or provider restart.

Discovery and delivery are distinct checkpoints. Provider pages advance only
after durable local handoff; a separate drain retries that outbox until server
acceptance. Acceptance still does not mean context integration completed.
Resumable PR discovery uses all PRs in creation order, without mutable base/state
filters; matching is local to the pinned incoming Git range. Creation and replay
share the same per-record lock so a delayed creator cannot erase a cursor.

The existing capture observer now wakes queued fetch work at startup and on its
existing ticks, including unchanged transcripts and failed capture/publication.
Worker locks and a bounded startup lease suppress duplicate launches. This
recovers an expired or crashed discovery worker while an observer survives or
restarts. Durable pagination progress keeps the worker's pause at one second;
exponential backoff applies only when work fails or stops progressing, avoiding
artificial lifetime expiry during a healthy long scan. A no-op fetch need not
produce a reference transaction, and an idle or
crashed observer is not an always-on service. Server reconciliation remains
essential. See `cli/FETCH_DISCOVERY_LIVENESS.md` for the tested lifecycle and
residual limits.
The current source association remains same-repository only; valid unsupported
fork webhooks keep their previous terminal-ignore behavior rather than retrying
their immutable payload forever. Fork association needs its own verified
cross-repository authority, not a branch-name fallback.

The server worker now consumes provider history independently of an online CLI.
It uses full-width PR IDs and handles HTTP-200 GraphQL rate errors. Primary
REST/GraphQL cooldowns are separate; secondary limits remain shared per
installation. Post-merge force-push events are excluded from participation,
without discarding valid earlier events on the same page. The current provider
head is never treated as the merge-time head.

### Phase 3 processing contract

- A dedicated due-job query includes old and completed PR jobs, not the UI's
  latest-100 list. A bounded tick isolates failures and skips revoked bindings.
  Provider PR discovery also paginates all PRs by creation order, filtering
  merged records locally; closing or updating a PR does not change its page
  membership through a mutable state/update-time filter.
- Before provider I/O, a small transaction reserves the next attempt and
  increments the checkpoint version. A crash, timeout or failed page commit
  cannot permanently monopolize the oldest scheduling slots. Expired readers
  cannot overwrite newer progress.
- Provider pages, normalized observations, dependent Git scans, participation
  evidence, matching cursors and the evidence revision commit together in
  PostgreSQL. A reservation is scheduling state only, not accepted evidence.
- History matching runs outside the write transaction against a read snapshot.
  The final write rechecks the graph revision, installation binding generation,
  repository ownership/archive policy and checkpoint version. It processes at
  most 100 observations and 100 publication candidates per matching step;
  a separate cursor resumes within a heavily populated historical Git head.
- Participation requires a genuine accepted publication and its original
  ordinary observation, an exact historical provider head of the same numeric
  PR/repository, and an ordinary observation no later than the verified merge.
  Publication arrival can be later. Conflicting context identities at the same
  head remain ambiguous; today's branch name cannot disambiguate them.
- Completed provider scans continue cycling stored observations for late
  publications. Transient provider refresh failures do not prevent matching
  already verified evidence; revoked access or inconsistent evidence still
  fails closed.
- These are additive, per-witness proofs. They are not integration receipts and
  do not move main, synthesize a publish event, or activate memory claims.
  Phase 4 must use a separate semantic contribution identity and revalidate
  stored proofs against current accepted history, including newly arrived
  conflicting identities, before applying context or memory.

At the phase 3 checkpoint, historical insertion, supplemental contributions and
proof-driven completion were still pending. The phase 4 checkpoint below records
their implementation and current limits. Customer-visible diagnostics, complete
transport/installed-service verification and actual #488 automatic recovery are
not closed by the phase 3 evidence tests. No customer graph has been manually
repaired.

### Verification checkpoint

- Backend `go test ./...` passed before final review corrections; the changed
  GitHub adapter/application paths were subsequently rerun with `-race`.
- PostgreSQL 16 tests with fsync enabled passed for observation/scans/promotion
  rollback, duplicate conflicts, dependent-publication rollback and revisions.
- Pager tests cover progress beyond 1,000 PRs, persisted restart, failed provider
  reads/handoff, and concurrent creation/replay; focused race checks passed.
- Final combined fetch/discovery/delivery/incoming regressions passed with
  `-race` (CLI delivery package 92.090s, application package 5.004s). Existing
  merged-PR ordering and ordinary-sync replay tests also passed. Go vet and
  public-tree/secret checks passed.
- The earlier concurrent CLI full-suite run did not pass: delivery, storage and application packages
  exceeded the default 10-minute package deadline, and native fixture handshakes
  failed during that run. The reported timeout-active tests and failed native
  cases passed in isolated reruns. The later serial rerun below passed; the
  earlier isolated successes alone were not counted as full-suite verification.
  No timeout or durability setting was relaxed.
- Primary checkout and live services/data are unchanged. No commit, PR,
  installation, historical backfill or deployment is claimed by this milestone.

Phase 3 checks on 2026-10-11:

- Focused backend application/domain/provider `-race` regressions passed,
  including two successive remote rewrites, delayed original publication,
  independent witnesses, restart between provider pages, and immutable source
  SHA/history/ref preservation.
- Reused context identities at the same historical head are rejected, including
  an ordinary observation whose final publication has not arrived yet.
- An old PR outside the latest 100 jobs still receives proof. Failed provider
  requests yield scheduling priority; 205 same-head publications resume in
  batches of 100 after worker restart. Post-merge branch reuse is excluded.
  Old resumptions retain priority over newly arriving jobs. Irrelevant
  post-merge events with nullable commit endpoints do not block provider pages;
  relevant missing endpoints remain an integrity failure.
- Established evidence can match a late source during transient provider
  failure, but provider revocation cannot authorize new proof writes.
- PostgreSQL rollback/stale-reader application tests and storage CAS, immutable
  duplicate/conflict, due-job, and rollback tests passed with `-race` against an
  isolated local PostgreSQL 16 cluster with fsync enabled. A failed evidence
  commit retains only its prior scheduling reservation, not a page cursor,
  observation, participation proof or evidence revision.
- CLI `go test -p 1 ./...` passed: delivery 494.307s, storage 166.066s,
  application 151.071s, native Codex 6.155s. Updated fetch/discovery/observer
  paths also passed focused `-race` checks (delivery 165.316s, capture 4.684s).
  Backend/CLI vet, PostgreSQL-tag vet, diff checks and public-tree/secret scan
  passed. Test-only PostgreSQL was stopped afterward. No live-data repair,
  installation, commit, PR or deployment has been performed.

### Association proof and processing states

Phase 3 must add an explicit association record, not relax the existing exact
publication validator. Its immutable identity includes the CXTHub repository,
provider repository ID, stable provider PR ID, original accepted publication
event, original capture target and verified merge SHA. Evidence references the
provider event IDs and observed head transitions; the branch display name is
not an identity or sufficient proof.

The initial proof rule is deliberately bounded: the original accepted
publication's Git SHA must be a verified historical head of that same PR. Do
not infer membership solely from an arbitrary ancestor, matching patch,
commit message, Git timestamp, or reused branch name. Multiple historical
publications are retained with their own provenance. Unknown ancestry is not
an invented order.

```text
observed -> awaiting evidence -> verified -> integration planned -> completed
                 ^                  |               |
                 |                  +-- conflict ---+--> needs evidence
                 +------- retry after transient failure
```

Each transition is restartable. Original capture upload, webhook delivery,
provider reconciliation and ordinary fetch discovery may arrive in any order.
New evidence wakes affected waiting jobs; a periodic bounded sweep recovers
lost wakes. Cursor completion means one discovery interval was examined, not
that its context was integrated or that future evidence may be ignored.

Provider pagination must be checkpointed per PR with lease/version fencing.
GraphQL's current head is not automatically the head at merge time. Compare it
with the verified merge receipt, bound force-push events to the PR's relevant
time interval, and never use post-merge branch reuse to attach unrelated work.
Relevant null commits, missing repositories, partial responses and provider rate limits
must preserve the cursor and existing evidence. No bounded page limit may be
treated as a complete history.

### Delayed source and current memory

Phase 4 implementation map:

1. Add an immutable semantic contribution ledger keyed by provider PR/repository,
   merge, destination/source context identities, original Git head and capture.
   Multiple publication/witness records can support one contribution; they must
   not duplicate its integration effect or replace the first completion receipt.
2. Revalidate participation against current accepted history, including late
   identity conflicts. Plan historical inclusion from pinned repository and Git
   evidence. Keep the current main ref unchanged when inserting old work.
3. Track desired/applied contribution generations so a completed PR job can
   process genuine supplements. Accepting proof must durably wake work; apply
   contribution, revision and job progress atomically with lease/version,
   repository revision, binding and policy fences.
4. Feed completed contributions into server-owned branch-context assembly and
   the virtual integration graph. Order by verified merge ancestry and retain
   natural source closures. Do not mutate snapshot-global grafts to place an old
   PR, infer Git order from timestamps, or widen `resolvePRSource` in place: its
   single frozen source, completion early-return and append-to-tip behavior are
   not a historical contribution contract.
5. Extend memory's historical participation check only. Keep original capture
   provenance, selected-code merge ancestry and scoped file/path evidence as
   separate checks. Rewritten PR membership cannot make an old memory claim
   automatically applicable to current code.
   Deduplicate conversation inclusion separately from versioned memory: a new
   accepted memory digest must retain its publication/hash provenance and be
   reassessed, not silently replace an earlier contribution's memory or be
   discarded because its conversation chunks were already included.
6. Verify actual main-ref preservation (not only a code-selection label),
   completed-PR supplements, duplicate witnesses, historical selections,
   PostgreSQL rollback/stale plans, and common Web/MCP results. Missing Git order
   remains unresolved rather than being replaced by a time-based guess.

A completed join is a permanent fact, but not a reason to discard a later
legitimate capture for the same PR. Phase 4 must handle supplemental source
contributions using stable contribution identities and a new integration
revision. Preserve the first completion receipt and all later target work;
never relabel the old capture or replace the latest main tip with the delayed
source. A worker computes its plan from a repository revision and validates
that revision again when applying it atomically.

Conversation inclusion, PR membership and current memory applicability are
three separate decisions. Historical conversation can be included while a
code-specific memory statement is excluded from the current-code view. Rebase
conflict resolution and partial revert therefore need scoped code evidence;
membership in a merged PR alone does not reactivate every memory statement.
Web and MCP must report the same completion and applicability revision.

### Phase 4 implementation checkpoint (2026-10-11)

- `pr_contributions` is an immutable semantic application ledger, separate from
  per-observation `pr_participations`. Its versioned identity includes the
  destination context identity, original head and capture. Duplicate witnesses
  cannot produce duplicate contribution effects. `pr_contribution_memories`
  retains independent publication pins, including explicit empty selections.
- PR-history reconciliation attempts contributions in its existing resumable
  bounded batches, including already-completed PR jobs. A contribution requires
  the verified merge on the destination's selected first-parent Git chain and
  available natural source closure. Missing ordering remains unresolved; it
  never invents an observation at a rewritten SHA.
- Preparation reads are outside the write transaction. Final PostgreSQL apply
  rechecks graph/evidence revisions, checkpoint version and GitHub binding and
  policy, then commits proof, contribution, memory pins, checkpoint and eligible
  job completion together. A currently leased legacy promotion worker is not
  stolen; its later retry consumes the persisted ledger. FS durability tests
  do not imply a cross-file ACID transaction.
- Historical contributions never move the active ref, mutate natural parents
  or grafts, or manufacture legacy `pr-merge` history with today's tip as the
  historical pre-merge position. Existing first receipts remain immutable.
  The common read model supplies virtual integration nodes and context roots;
  multiple captures at one verified PR merge share one visual operation.
- Current eligibility is revalidated against the complete accepted history.
  Late competing branch evidence moves a contribution to review without erasing
  its applied fact. That fact remains a replay fence: loss of current eligibility
  cannot fall through to legacy append and undo a later reset. Independent valid
  head evidence for the same capture remains usable.
- The exact-final-head promotion path now also checks known Git ordering. A
  proven older PR leaves the actual current ref unchanged, not merely its code
  label. Unknown/unrelated ordering must not become an append based on arrival
  time. First legacy initialization without any code association is a distinct
  compatibility case, not historical ordering evidence.
  Missing ordering uses `waiting / ordering_evidence_pending`, independently of
  missing source finalization. Its exponential delay caps at 256 seconds and is
  not reset by replaying an already accepted publication. Scheduled attempts
  recheck newly scanned Git evidence or position observations without a customer
  retry action; missing evidence never authorizes an append. The synchronous
  compatibility endpoint returns a pending conflict, not an internal failure.
- Memory pins are not the mutable `Snapshot.MemoryHash`. A causal successor can
  supersede an older pin; incomparable versions are not blindly unioned, and
  rewritten membership does not bypass selected-merge ancestry or scoped file
  checks. Conversation deduplication cannot discard a new memory version.
  Matching a legacy completion uses the semantic destination/capture/merge key,
  not only an event ID; otherwise a coalesced receipt could bypass the pin. A
  source independently reachable in natural main ancestry keeps its ordinary
  memory semantics. Unresolved contribution-only memory returns a scoped
  conflict, not invented summary prose or a silent mutable-pointer fallback.
- Grouped graph operations carry their source arms explicitly. Birth-node
  substitution matches the original source identity rather than replacing the
  last parent; late supplements therefore remain connected and retain their
  source-branch label. Original stored parents remain unchanged.
- Selecting a contribution merge resolves its recorded merge code through the
  shared memory-position query. A ledger ID is not looked up as a synthetic
  history event. Invalidated participation remains unavailable for that selected
  ID instead of falling back to another code position; unrelated snapshots do
  not acquire the association.

This checkpoint is not a claim that every historical gap has been repaired.
Automatic application currently needs a verified selected destination code;
an absent code position, unavailable provider history or unsupported source
scope remains unresolved. An authenticated merged webhook can prove the final
head, whereas an untimed synchronize hint or the provider's current head cannot
prove the original merge-time head. Rich public contribution diagnostics,
installed-binary verification and actual #488 recovery remain rollout work.

### Phase 4 validation closure (2026-10-11)

- Stable final backend source: `go test -p 1 ./...` passed, including HTTP/MCP
  delivery contracts and architecture checks; `go vet ./...` and PostgreSQL-tag
  vet passed. PostgreSQL-tag API/MCP binaries build without installation.
- Isolated PostgreSQL 16 with fsync enabled: contribution ledger/pin constraints,
  rollback, duplicate/stale workers and final history-page apply passed. The
  combined PostgreSQL/application/domain focused suite also passed with `-race`.
- Regressions cover late rewritten captures, completed-PR supplements, pinned
  memory supersession, ambiguous evidence, legacy-receipt coalescing, preserved
  resets, exact selected-event resolution, and recovery after 22 ordering waits.
- Frontend: 24 unit files passed, including Go-produced graph contracts.
  The complete UI-regressions browser file passed 63 tests; two tests requiring
  full-stack mode were skipped. Grouped birth/supplement SVG paths are checked
  across reload, not only by counting nodes.
- Type checking, build, i18n key/interpolation parity and frontend dependency
  checks passed. Existing duplicate-copy and bundle-size warnings remain.
  Public-tree/secret scanning and `git diff --check` passed.
- Independent review found and corrected invalid-ledger replay, alternative-head
  poisoning, legacy memory-pin bypass and grouped source-arm replacement. These
  checks do not certify installed services, real provider access, or actual #488
  recovery. No live graph data, installed binary or primary checkout was changed;
  all implementation changes remain uncommitted in the development worktree.

### Phase 5 integration checkpoint (2026-10-11)

- The durable generic merge queue retains the original Git range, destination
  identity, rewrite observations and frozen candidate plan across failed reads,
  uploads, appends and lost acknowledgements. Replays never substitute a newer
  `ORIG_HEAD` range. The imported focused merge/status tests passed.
- Server-owned context inclusion, not only raw DAG reachability, now lets the
  CLI acknowledge a historical PR contribution without appending it again or
  moving the current main tip. A contribution excluded or under review at the
  selected code position must not fall through to legacy append. Focused tests
  passed for branch reuse, revoked query access, changed local scope and a late
  source after a completed successor.
- Automatic merge-queue wake/retry and conditional append are being integrated.
  The append precondition must fence both the selected target and graph/evidence
  revision: a ledger change can leave the raw target unchanged. Older servers
  must reject this conditional operation rather than ignore an optional field.
- PostgreSQL HTTP/MCP transport tests now compare context list, project memory,
  effective memory and exact stored archives at one revision. Unauthorized and
  anonymous reads are denied; newly ambiguous participation withdraws current
  inclusion without deleting historical records. The fixture uses validated
  synthetic evidence; provider ingestion is tested separately. The targeted
  PostgreSQL transport test passed, including three race-enabled runs.

The live services were inspected read-only. They use PostgreSQL and older
installed binaries; GitHub App is disabled and no usable installation binding
is exposed. The existing #488 job is still `source_finalization_required`.
Its original publication and capture survive, but there is no final-head
publication. Its publication memory pin is explicitly empty: the capture's
current memory pointer is not a substitute. Real recovery requires enabling the
ordinary App installation/repository connection and installing the tested code;
it has not been certified or repaired by editing data. Missing operator setup
does not excuse unfinished local tests or implementation.

Generic Git merges without verified PR participation remain a distinct case:
when an older source appears after a completed successor, the queue does not
claim historical insertion merely because the conversation object exists.
It can acknowledge independently verified server inclusion. The qualified
prospective path now has its own server contribution operation, described below;
historical records without that evidence remain unresolved. The PR contribution
ledger alone does not close those generic cases.

### Generic integration closure: acceptance contract

The generic queue must not infer a completed integration from `[git ...]`
message text, mutable graft reachability, or the absence of another local queue
entry. A different client can already have advanced the server. Target/revision
CAS prevents concurrent overwrite but does not establish semantic Git order.
Without a verified contribution from the server operation, unknown or newer
server code positions retain the queued obligation rather than append an old tip.

The operation has these boundaries:

1. Resolve an accepted publication by its immutable publication ID, original
   capture, full original Git object ID and source context branch identity.
   Local range reports and message labels can select discovery candidates only.
2. Verify the repository's configured provider evidence for the destination
   identity and integration commit. Establish the integration's order against
   the destination's verified Git history without interpreting arrival time as
   order. Missing or unsupported provider evidence remains retryable; a client
   assertion does not become a server-verified GitHub fact.
3. Persist a generic contribution and its explicit original memory pin, including
   an empty pin, with a stable identity independent of retries. Recheck write
   authorization, provider binding, read revision and destination identity in
   the repository transaction. Commit contribution, completion and revisions
   together; never move an older source to the current main tip.
4. Share the contribution ordering and pinned-memory projection with PR
   contributions rather than duplicate the existing memory-selection engine.
   Retain separate PR participation requirements; a generic operation cannot
   masquerade as evidence that a rewritten capture belonged to a PR.
5. Publish exact historical acknowledgements separately from current inclusion.
   An acknowledgement covers only its source, destination identity and full
   integration SHA in the frozen incoming range. A receipt for one source
   cannot discharge other sources or prove that an empty candidate scan was
   exhaustive. Reset/revert changes current applicability, not original delivery.
6. Test A arriving after B from another client with no local B queue, reset after
   completion, lost responses, late memory pins, source/branch reuse, revoked
   installation access, unavailable Git objects and mixed PR/non-PR ranges.
   Assert unchanged selected tip/natural parents and preserved B content/memory.

This subsection is the acceptance boundary, not a claim that every item has
passed. The qualified-read contribution operation and public-query consumption
of its shared memory engine are implemented below. Complete empty-range discovery,
late memory supplements and admission without a same-page baseline are still
outstanding.

### Phase 5 review corrections and generic foundation (2026-10-11)

Historical delivery acknowledgement now has an explicit query field. The CLI
accepts it only for the exact source, destination context identity and full
integration SHA in its frozen incoming range. Reset can exclude a delivered
contribution from the current context without scheduling that contribution for
another append. A mixed range still retains every unacknowledged source, and a
receipt for one source cannot close an empty discovery result. When the server
supports semantic inclusion, raw graft ancestry cannot override an excluded,
review or missing contribution. Before appending, actual local Git evidence must
also prove that the selected server code is not ahead of the incoming range;
replace refs, graft files and lazy fetching cannot supply that proof.

The versioned conditional-append endpoint fences branch identity, expected
target and Graph/Evidence revision in the same PostgreSQL transaction as the
write and authorization recheck. An older server or FS adapter cannot silently
fall back to an unconditional append. Pending-only revision changes do not
invalidate this semantic precondition.

Generic non-PR implementation has started, but is not an enabled end-to-end
feature yet:

- `GitContribution` and independently pinned `GitContributionMemory` records
  have immutable versioned identities and FS/PostgreSQL storage. Replay,
  conflicts, cancellation, policy fences, concurrent writers and PostgreSQL
  rollback/savepoint recovery have focused tests. Revision advancement remains
  application-owned, not an adapter side effect.
- Exact ordinary publication evidence is indexed separately from PR membership.
  A publication cannot prove itself; its original repository, context identity,
  native branch, worktree, Git head and capture must match an accepted ordinary
  observation. Rename/archive labels are not capture evidence.
- A bounded Git traversal locates the actual first-parent introduction boundary,
  rather than assigning the current tip or the latest arriving commit. Missing
  history is not absence. Cycles fail integrity checks. Budget exhaustion has a
  distinguishable limit error; a resumable traversal or explicit limit state is
  still required before claiming liveness for oversized histories.
- Read-bound destination candidates and read-only inspection preserve the
  exact destination identity, qualified observation, original publication,
  matching ordinary observation IDs, capture and memory pin. They do not issue
  contribution records, join proofs or completion receipts. Inspection cannot
  move refs, rewrite natural parents or use a newer mutable memory pointer.
  A candidate's current context identity is not proof that the incarnation
  existed during that provider read. Admission persistence now has a separate
  transaction boundary; authoritative join application is still pending.
  These candidate types cannot themselves emit completed joins.
- PR and generic contributions normalize into one memory-selection engine.
  Existing PR pin JSON/IDs remain unchanged. Tests cover original, explicitly
  empty and unknown memory, mixed evidence deduplication and arrival-order
  independence. Public queries still consume PR contributions only until the
  generic authoritative write and revalidation path is complete.

The independent review fixed the remaining worker contract:

1. Reuse existing authenticated push and provider-head scan boundaries; do not
   introduce another provider crawler. Stamp installation and numeric repository
   identity only when explicitly authorized. Legacy/PAT observations remain
   unqualified, and existing immutable rows retain their original identity.
2. Bind a fresh qualified head to the exact destination context identity under
   a repository transaction. A delayed webhook cannot be attached to today's
   reused branch name merely by matching text. Preserve historical witnesses;
   the newest inserted observation is not necessarily the current head after
   an A-to-B-to-A reset.
3. Persist bounded discovery/work checkpoints and deferred candidates. Reuse
   fixed-generation catalog baseline/delta cursors for publication discovery;
   advance a complete checkpoint only after the final page and durable handoff.
   New publications, memory supplements and successful evidence indexing must
   wake the work. Use Graph/Evidence revisions for application CAS, not as an
   endlessly resetting discovery cursor.
4. Require causal source admission as well as code introduction. A branch
   created at commit A after main already contains A has not joined main.
   Replaying an older main transition that introduced A cannot establish that
   later branch's participation either. Before/after ancestry alone, client
   timestamps, insertion order and branch-name matches are insufficient.
   The ancestry-only draft proof constructor was removed before any worker or
   public query was wired to it. Its replacement must retain exact ordinary
   capture/lifecycle evidence and provider-qualified destination incarnation;
   first observations and already-reachable candidates are discovery, not joins.
5. Commit proof, contribution, pin, completion and revision together after
   rechecking work version, policy and binding. A server-owned range/watch
   obligation can acknowledge delivery ownership, not semantic completion; it
   must continue discovering late publications. No client is required to remain
   online for that owned work.

The prospective generic admission boundary is deliberately narrower than PR
participation. Freeze exact ordinary capture IDs and both lifecycle anchors
before provider I/O; a publication can arrive later, but a newly discovered
ordinary capture cannot be retroactively added to that frozen input. Preserve
original capture C at A separately from a carried source tip S. When A is already
the main fork point, accepted ordinary evidence must explicitly associate the
same capture C with S in the same context incarnation, native branch and worktree.
Verify A-to-S ancestry without relabeling the original publication or memory pin.
Ancestry and branch names alone do not establish that carry. Generalized carry
through a different capture is not implemented.

Verify an authorized source tip S and a destination baseline whose complete
ancestry does not contain S, not necessarily A. Persist that admission before
starting its consuming destination read. A later containing head must continue the baseline's
first-parent path, and the introduction must lie strictly inside that range.
The bounded range verifier now enforces this structural condition, including
fast-forwards, missing evidence, discontinuities and cycles through the source.
It is not by itself a context-join proof.

Immutable observation IDs deduplicate identical ref facts, including A-to-B-to-A.
They cannot identify which read happened after an admission. Preserve a separate
successful scan-claim/page occurrence, and never substitute insertion order or
a higher but already-running claim for a read started after admission. Provider
pages are not an atomic snapshot of all Git refs. Both incarnations, frozen
inputs, provider binding and repository revisions require transactional fences.
If two source incarnations own the same candidate head, membership is ambiguous;
do not pick one by name or arrival time. Already-integrated first observations
or missed source-admission windows remain unproven unless independent PR or
other authoritative evidence supplies the missing relationship.

Qualified read-occurrence persistence is now implemented in the existing head
reconciler, not a second polling service. Migration `0083_git_head_receipts.sql`
stores one immutable receipt per accepted claim version. It retains at most 100
ref/head/observation references, provider installation/repository identity and
the page boundary; it does not copy conversations or the history catalog.
PostgreSQL commits the receipt, observations, endpoint indexing jobs and scan
checkpoint together. Expired, completed and superseded claims fail before writes.
A-to-B-to-A produces three receipts but only two immutable head observations.
Bounded reads use exact repository/origin and an ascending version cursor.

Empty or providerless pages do not issue a qualified receipt or prove branch
absence. Development FS has per-file persistence only: a crash can advance its
checkpoint without a receipt, and that occurrence is not reconstructed. Causal
application must require PostgreSQL repository transactions. Receipt membership
also does not bind today's context incarnation to the historical read. Candidate
inspection therefore returns `GitDestinationCandidate`, not an admitted join.

Admission persistence now has an automatic prospective-admission path in the
existing head reconciler; it is not a completed generic-join worker.
`GitContributionAdmission` freezes original
and carry observation IDs, both lifecycle anchors, context identities, capture,
original/source heads, provider scope and Graph/Evidence revisions. Preparation
originally required qualified source and destination heads from the same receipt page,
complete A-to-S ancestry and proven absence of S from the destination baseline.
Missing heads on another page do not establish absence. Different captures,
ambiguous source owners and newly arrived ordinary evidence are not silently
added to frozen inputs. The ordered source-read extension below removes the
same-page limitation without treating separately observed pages as one snapshot.

Migration `0084_git_contribution_admissions.sql` persists the immutable admission
and referenced read receipt. The PostgreSQL-only write requires an owned active
repository transaction and locks the scan-claim row until its outer commit. It
records the current version, including an in-flight claim; a later consuming
read must start beyond that watermark. Comparing only the baseline receipt's
version would permit an already-started stale read. A separate semantic key
excludes receipt versions and global revisions, so repeated scans retain the
first admission instead of refreshing its watermark indefinitely. FS cannot
claim this capability. These storage operations do not advance revisions by
themselves or write context refs/contributions. The internal admission command
also leaves public repository revisions alone; the enclosing head-page finish
advances evidence once. Replaying an admitted intent does not invalidate queries.

The internal application boundary rechecks frozen inputs, refs, lifecycle and
Graph/Evidence revisions, verifies capture closure, and binds the provider
identity to its authorization fence before persistence. An actual PostgreSQL
transaction test combines head-scan finish, receipt, admission and revision;
revocation, stale inputs and failed commit work leave none of those partial
results.

### Durable discovery and prospective admission

Migration `0085_git_association_discovery.sql` stores the catalog checkpoint and
accepted original publications in one repository transaction. The reconciler
reads one bounded baseline/delta page per pass, including while a provider head
read is not due. Partial pages retain their fixed epoch/through position; reset
starts a new baseline without deleting discovered originals. An idle catalog
does not rewrite the cursor or invalidate public queries. Local catch-up may
run each second; provider claim eligibility and backoff remain unchanged.
One repository's failed head claim no longer prevents later repositories from
being processed in the same pass.

Migration `0086_git_admission_match.sql` persists a separate matching cursor per
repository, origin, numeric provider identity and provider page. Discovery is
not admission: a discovered publication can still lack ordinary or provider
evidence. Each matching sweep fixes an upper publication ID, processes at most
eight publications and eight source/destination tuples per head read, resumes
destination fan-out, then wraps. Late IDs below the cursor enter a later sweep;
the cursor is not an arrival watermark. Separate page cursors prevent unrelated
provider pages from permanently steering a publication away from its source.

Before `ListGitHeads`, one coherent read freezes the ordinary-evidence index,
current context refs, lifecycle projection and repository revisions. Selecting S
from the response can only select an association already present in that frozen
index. Newly arrived ordinary evidence cannot enlarge the pre-read input. The
single-item command and batch path share the same branch-identity resolution and
admission verification; batch revalidation loads the index once, not once per
candidate. Complete local code evidence is checked outside the write transaction;
no additional provider crawler or provider HTTP inside a write is introduced.
Equivalent ordinary/carry witnesses use a canonical bounded proof subset in an
admission; the complete original index and history remain intact for ownership
ambiguity and revision checks. Independent review found that copying more than
128 equivalent witnesses previously rejected the entire batch and stalled its
cursor. The bounded selection fixes that case without inventing evidence or
discarding original records; a 129-witness regression includes a later eligible
publication to verify forward progress.

The accepted head receipt, prospective admissions and matcher cursor commit in
the same existing evidence transaction. Stale candidate inputs defer without
blocking authorized head ingestion; revoked provider access rejects the whole
page. Storage failure rolls the page, admissions and cursor back together while
previously discovered originals remain queued. Repeated admission preserves the
first watermark. Traversal-limit exhaustion yields after the affected tuple so
later tuples can obtain a fresh bounded read budget rather than being starved by
the same large candidate on every sweep.

At that checkpoint, source and destination had to appear on the same qualified
page. The ordered source-read extension below now handles separate pages;
separate-page reads are still never unioned into an atomic observation. Discovery
is incremental, but an eligible matching batch still loads the repository's full
ordinary-evidence index; an incremental index is a remaining performance task.
The qualified same-page path now has consuming-read application, original-memory
pin publication and common query integration, as described below. The new
supplement path and customer-facing generic-work diagnostics are also recorded
below; this is not a claim that all Git workflows or legacy records
can already be associated automatically.

### Bound consuming reads and automatic application

Migration `0087_git_context_heads.sql` binds each qualified provider head to the
context branch incarnation and target frozen before that provider request. The
binding and original receipt commit with the head page. This is not a current
ref lookup performed after the response: a branch created during I/O cannot
become that read's destination. Later application can use the original bound
read after a reset, source archive or delayed code indexing. A destination rename
must have an accepted binding-parent chain; a reused name cannot inherit an old
incarnation's evidence.

The application locates the actual first-parent introduction between the admitted
baseline and later qualified head using complete immutable code evidence. Its
transaction verifies current repository/provider policy, frozen revisions and
original publication before writing the generic contribution, original immutable
memory pin, completion proof and one evidence revision. Natural parents and refs
do not change. Explicit empty memory remains empty; a newer mutable snapshot
memory pointer is never substituted. A proof is unique per admission; repeated
delivery returns that proof without another evidence revision.

Migration `0088_git_contribution_work.sql` queues an admission's automatic work
atomically with its persistence, including backfill for existing admissions. The
existing Git scan worker also consumes this queue; no second provider crawler
is added. Each claim handles at most one bound head and records a finite sweep's
upper read version. Missing code, absent source or a discontinuous range advances
within that sweep; after draining, it resets to the admitted watermark and waits.
Thus later indexing can make an earlier pre-reset read usable without permanently
losing that read or starving other admissions. Retries are leased, version-fenced
and backed off. An incompatible provider read is skipped within the sweep without
applying it; revocation and write-time conflicts do not advance the cursor. This
distinction prevents an older foreign-installation read from permanently blocking
a later compatible read after the original binding is restored.
Integrity or traversal-limit failures become durable attention
states rather than a success or an unbounded traversal. Applying the contribution
and marking work complete share one outer transaction; a failed completion write
rolls back the contribution, pin, proof and revision too.

PR and generic contributions now use the same branch-context, graph, memory and
code-position projection. Mixed PR/Git evidence for the same capture, destination
and integration commit deduplicates that capture and its memory while retaining
independent original event IDs. Different captures introduced at the same
destination/commit share one graph join with all source arms, rather than an
invented sequence of joins. A known PR label is retained when generic and PR
records share that join. Reading projections performs no provider
requests or storage mutations. The accepted proof establishes historical
completion; current Git inclusion still follows the selected code position.
Pre-application ambiguity checks remain strict, but new unrelated branch
ownership after completion must not revoke that immutable completion. Unknown
generic integration timestamps remain unknown and are not displayed as year 1.

Local checks for this increment include PostgreSQL apply/replay, source archive,
destination rename, late ambiguity before apply, policy/revision fences, concurrent
single effect, outer rollback, delayed indexing after reset and completion-write
failure. The broader PR/generic/query race run passed (app 208.766s, store
122.411s, domain 1.670s) before the localized foreign-provider skip correction.
Four PostgreSQL public-query tests passed with race detection, including actual
ordered captures, original memory pins, reset, later ownership and mixed-ledger
deduplication. Work-store race checks passed (81.884s), covering atomic enqueue
and migration backfill, frozen sweep bounds, lease/CAS rejection, rollback, fair
rotation, policy fencing and indexed unique proofs. Independent review found and
corrected the foreign-provider cursor stall and generic same-commit grouping.
The provider regression passed four cases (2.603s); a fresh domain grouping race
run passed (1.294s) and combined generic apply/query/worker race run passed
(8.997s). Full normal backend tests passed (application package 422.962s) before
those localized corrections. Web unit tests passed (26 files), as did typecheck,
PostgreSQL-tag vet and API/MCP builds. No installed service or live data changed.

### Late original-memory supplements

Migration `0089_git_contribution_supplements.sql` queues a new accepted pinned
publication against its already-proven contribution. The immutable repository,
source branch incarnation, original capture and original Git head must match;
current branch names, graph proximity and mutable snapshot memory pointers are
not sufficient evidence. The application also rechecks the publication's exact
ordinary worktree evidence. An archived source can still receive a historical
pin without reopening its ref or repeating its context integration.

The queue handoff is atomic both when the publication arrives first and when
the contribution proof arrives first. Backfill follows the same matching rule.
The first retained proof anchors the contribution; later duplicate proof IDs
cannot retarget a pending lease. Jobs have a stable repository/contribution/
publication identity, versioned leases, bounded retries and explicit attention
states. Invalid linked evidence is settled through a claimed job rather than
permanently blocking valid later jobs; malformed queue storage itself remains
fail-closed.

The existing contribution worker consumes supplements. Blob hash/source checks,
current provider policy, prepared repository revisions, immutable pin creation,
one evidence revision and job completion share the write transaction. A failed
completion write rolls the pin and revision back. No provider HTTP runs there.
Missing original memory waits; corrupt content is not substituted. Web/MCP use
the existing shared causal memory projection: a pending exact publication is
unavailable, not silently replaced with an older or mutable digest; after the
pin commits, a verified causal successor supersedes the older version.

PostgreSQL race checks cover automatic discovery/replay, missing and corrupt
memory, concurrent expired/current workers, graph/evidence drift, revoked
bindings, source archive, completion-write rollback and shared-query behavior.
The focused application run passed in 4.324s; the related PR/generic/query run
passed in 25.410s before the subsequent source-read extension. Domain/storage
checks also cover ID parity with migration SQL, duplicate handoff, publication
arrival order, overflow, policy and transaction fences.

### Ordered source reads across provider pages

The reconciler freezes publication/ordinary evidence and context identities
before I/O, then reads up to eight distinct exact source refs before reading
the normal destination page. It uses GitHub's exact
[Get a reference endpoint](https://docs.github.com/en/rest/git/refs#get-a-reference),
not prefix matching. Source reads share at most ten seconds and at most half of
the remaining reconciliation budget, inside the existing thirty-second window
and the existing provider cooldown. Thus slow local preparation cannot give
optional lookups all the time that remains for the ordinary destination page.
They do not add an independent crawler or extend product timeout limits.

The qualified receipt stores `prior_source_heads` separately from `heads`.
This records ordering, not an atomic remote snapshot: a source was observed
before the destination baseline. Only actual paged heads may establish a
destination binding or participate in destination fan-out. If the page itself
contains the source, that later observation takes precedence. Older receipts
are never unioned; missing/failed reads prove neither absence nor deletion.
An empty page still provides no destination receipt.

Admission still requires complete original-to-source ancestry, proven source
absence from the destination baseline, exact frozen local inputs and a future
consuming read beyond the admission watermark. A source change during I/O does
not relabel the original capture. Receipt, both observation sets, prospective
admission and matcher/page cursors commit together. The optional receipt field
preserves legacy JSON and content IDs when absent. Providers/stores without the
explicit capability retain the qualified same-page path.

Local PostgreSQL tests exercise actual separate-page admission followed by the
automatic integration worker, original empty memory pin, unchanged refs,
deduplicated source calls, unavailable/wrong refs, stale pre-read state and joint
cursor rollback (2.991s). Domain/provider tests cover exact names and escaping,
redirect refusal, shared cooldown, mixed identity rejection, page precedence
and forbidding a prior source from serving as a destination. Storage source and
legacy receipt race regressions passed (2.890s). Independent review confirmed
the ordered-read contract and prompted explicit tests for contradictory page
heads, optional timeout progress and fail-closed authorization denial. The
combined domain/application/storage/provider race run passed (1.461s / 71.270s /
48.088s / 1.945s) before the localized authorization/budget refinement. The
subsequent focused application run passed (14.141s), including real optional
timeout and parent cancellation. PostgreSQL-tag vet and temporary API/MCP builds
passed again after the budget refinement.
No installed service has changed.

Validation at this checkpoint:

- Full serial CLI suite passed before the generic foundation; the full serial
  backend suite also passed after the read-occurrence integration, including
  normal/PostgreSQL-tag vet. A rerun against a stable tree replaced an earlier
  interrupted verification that overlapped the candidate type's file rename.
  Focused CLI merge/order/acknowledgement race tests passed after the safety
  corrections. PostgreSQL conditional-append, transport and acknowledgement
  race tests passed.
- Browser UI regressions: 64 passed and two explicitly skipped. Unit tests,
  typecheck, build, i18n, public-tree and secret checks passed at that checkpoint.
- Actual PostgreSQL sync E2E initially failed: its deliberately fake Git host
  provided no server ancestry, so the new ordering guard correctly waited.
  A test-only compiled helper now reads the fixture's actual immutable commits,
  trees and every parent diff through `GitScans`. Production wiring, ordering
  checks and timeout values are unchanged. An intermediate rerun passed 197 assertions.
- That intermediate rerun emitted three phase-E hook warnings. Investigation found old
  fixture requests omitted the session ID required by scoped session notices;
  it was not document corruption. The fixture now provides scoped session IDs
  and asserts clean stderr; a real notice-service regression covers Claude and
  Codex, terminal isolation and once-only delivery. The fresh PostgreSQL E2E
  passed 203 assertions with zero hash-mismatch warnings; all three phase-E
  stderr files were empty.
- Generic candidate/identity and common-memory race tests passed, including
  PostgreSQL storage rollback tests on a disposable database with fsync enabled.
- Qualified receipt storage/application race checks cover A-to-B-to-A, duplicate
  and stale claims, cancellation, nested/outer PostgreSQL rollback, provider
  disconnect during I/O, legacy/providerless compatibility and scoped reads.
  Range checks cover shared read limits, cancellation during traversal,
  incomplete after-side history and cycles through the source. Code inclusion
  at an already-existing main commit does not write contributions, memory pins,
  merge history or refs.
- Carried-tip tests preserve original A/capture/memory provenance while requiring
  explicit same-capture ordinary evidence for S. Admission tests cover real
  PostgreSQL rollback, authorization/installation scope, active-transaction
  ownership, claim-row lock retention, already-running claims, semantic-key
  replay, concurrent writers and bounded reads. The combined domain/app/store
  race run passed after provider/fence pairing was enforced.
- Discovery race tests cover concurrent catalog reads with one CAS winner,
  partial baseline/restart/delta, catalog reset, canceled and malformed pages,
  immutable-original handoff, rollback and independent head ingestion. Matcher
  integration tests execute the real reconciler with PostgreSQL: automatic
  admission, first-watermark replay, late lower-ID publication, independent
  provider-page sweeps, stale ordinary/ref inputs, binding revocation and joint
  receipt/admission/cursor rollback. A batch reads history once before provider
  I/O and once for write-time revalidation, including an eight-candidate batch.
  The combined focused domain/app/store race run and normal/PostgreSQL vet passed.
  After the witness-limit correction, the final focused race run passed again
  (domain 1.265s, app 42.461s, store 16.391s); PostgreSQL-tag vet and API/MCP
  builds also passed. The broader existing PR/contribution/query race regression
  passed in 159.097s before that localized proof-subset correction. Built binaries
  remain temporary verification outputs; no installed service was replaced.
- The latest aggregate CLI rerun hit the default ten-minute timeout in the
  delivery/cli package while a newly started fixture test was running. This run
  is not recorded as a full-suite pass. All 440 top-level tests subsequently
  passed exactly once across eight disjoint exact-name batches, retaining the
  default timeout. Combined package runtime was 727.753 seconds; the longest
  batch was 216.337 seconds. The named test also passed independently. Other CLI
  packages and the separately rerun CLI vet passed. Product timeout limits and
  package test sources were not changed to accommodate the aggregate duration.

No live service installation, customer data repair, commit, PR or deployment is
claimed by this checkpoint. The real #488 recovery and the subsequent requested
plans remain open.

## Required tests

- Original capture at A; provider rewrites A to B to C; final PR merge at M:
  automatic integration retains A as capture provenance and resolves C/M as
  provider history, with no native capture invented at C.
- Webhook lost but local fetch observes A to C; both input paths recover the same
  operation. Server-only reconciliation also works without a customer online.
- Synchronize, close, push and publication events duplicated and delivered in
  every relevant order, including close before source upload.
- Multiple customers and server workers race; one durable semantic result,
  no cycle, no lost successor, no duplicate memory contribution.
- Worker crash before commit, after commit/before acknowledgement, and after
  lease expiry; deterministic restart and no stale-worker writes.
- Fetch only, no-op fetch, prune, non-default refspec, secondary remote, bare
  repository, shallow history and corrupt/truncated local journal.
- PR head changes during provider reads, branch rename/reuse, forked repository,
  origin change, installation disconnect and permission revocation during apply.
- Unavailable intermediate commit, partial provider pagination, conflicting
  observations and missing original capture do not become fabricated success.
- Rebase conflict resolution, partial revert and full revert preserve dialogue
  history without treating all old memory claims as current code facts.
- Delayed A after already completed B preserves B's active position; temporary
  errors and worker restarts cannot bypass ordering checks.
- Customers need no database credentials, manual repair or extra pre-merge step.
  Existing retention, session archive and graph-folding rules remain unchanged.

## Verification and rollout

### Conditional append storage requirement

Versioned conditional append requires PostgreSQL. The service requires both
`RepositoryTransactions` and repository revisions so current authorization,
branch identity, expected target and Graph/Evidence revision checks share the
transaction that applies refs, grafts and pending-pointer changes. Pending-only
revision changes do not invalidate the precondition.

`FSStore` deliberately does not implement `RepositoryTransactions`. It rejects
conditional append without mutating the repository; automatic merge delivery
can therefore remain pending on FS even when ordinary legacy append works.
Neither FS nor an older server returning 404 may trigger an unconditional
fallback. Do not weaken the transaction requirement to make an FS test pass.

The sync E2E suite invokes real post-merge and publication hooks. Use a disposable
PostgreSQL 16 database for conditional-append/automatic-merge coverage:

```bash
CXT_E2E_DSN="${DISPOSABLE_POSTGRES_DSN:?set a disposable PostgreSQL 16 DSN}" make e2e-sync
```

`CXT_E2E_DSN` selects the PostgreSQL-tagged server and its test database. Without
it, the suite uses FS; that run covers legacy compatibility, not conditional
append ACID or successful automatic merge delivery. CI has separate FS and
PostgreSQL sync E2E lanes. Never point either test DSN at production data.

### Customer-visible automatic-link status (2026-10-11)

The read-only application query now projects three bounded ledgers:
`publication` discovery/admission, `integration` work, and `memory`
supplementation. REST `GET /repos/{repoID}/git/associations`, MCP
`context_link_status`, and the collapsed graph diagnostics panel consume that
same projection. Repository viewer authorization is required; none of these
reads call a provider, enqueue work, repair data, or move context/code refs.

An exact immutable integration proof or memory pin determines historical
completion, even when worker cleanup still says waiting. Scheduler completion
without its required effect fails integrity validation. Expired leases render
retrying without a write. Publication admission means at least one prospective
admission, not a completed merge or all possible destinations. The response
explicitly leaves current applicability unevaluated; empty pending pages and
recorded checkpoints never claim global capture completeness.

PostgreSQL uses repository/origin-scoped keyset reads and indexed existence
checks, validates accepted publication and linked admission/receipt/proof/pin
identities, and returns at most limit plus one records. The application reads
metadata and evidence in the same repository read snapshot. REST defaults to
30 records (maximum 100); MCP defaults to 10 (maximum 20), trims pages to its
byte budget, and binds cursors to repository, record kind and filter. Earlier
concurrent IDs require a fresh pagination pass.

Local verification at this checkpoint:

- Real PostgreSQL application lifecycle tests passed (1.092s), including late
  memory waiting-to-completed, exact pin authority, corrupt evidence rejection,
  cursor coverage, unchanged refs/parents/history and no provider I/O.
- Focused race regression passed across domain, application, PostgreSQL store,
  REST and MCP (1.253s / 11.834s / 9.923s / 2.510s / 3.093s). It includes
  supplementation and contribution-worker regressions as well as the new reads.
- Full domain, REST and MCP suites passed (5.078s / 16.929s / 1.468s).
- Combined browser run passed 70 tests (1.2m); the two real API/remote OAuth
  full-stack tests were explicitly skipped in this non-full-stack run.
- Frontend 26 unit suites, architecture check, i18n and production build
  passed. Existing ten translation-duplication and bundle-size warnings remain.

The diagnostic reads are not the automatic recovery mechanism: workers remain
responsible for forward progress. Installed API/MCP/CLI services and live #488
have not changed. Incremental ordinary-evidence indexing and installed-binary
verification remain required before declaring the overall plan complete.

Independent review corrections and subsequent checks:

- Reopening the panel rereads its current page without interval/focus/reconnect
  polling. A last-read timestamp distinguishes a cached observation from live
  completion. Missing destination fields no longer assert that a destination
  was never recorded, and an unavailable reconciliation omits the fake page 0.
  Eight focused browser tests passed after these corrections (5.8s), followed
  by a fresh production web build.
- Legal maximum-length branch names containing HTML-sensitive characters could
  exceed MCP's single-row budget solely through JSON escaping. The tool now
  serializes those characters losslessly without HTML escaping; the JSON is
  tool text, not HTML. The complete MCP suite and exact-label continuation
  regression passed (1.293s).
- Two real full-stack browser tests passed separately (4.9s), using a new
  disposable PostgreSQL database and separate API/MCP processes. They exercise
  OAuth consent, PKCE, revocation and all three status kinds through both
  transports, confirming exact REST/MCP parity for an unconfigured empty repo.
  Nonempty completion/pin behavior is covered by the application PG lifecycle
  tests, not inferred from this empty-repo smoke. An initial smoke fixture used
  an invalid repository name with spaces; correcting the fixture fixed it.

### Generation-fenced reuse outside write locks (2026-10-11)

The first indexing improvement removes application-level full-history reloads
from reconciliation's final write, standalone admission persistence,
contribution application and memory supplementation. Preparation still reads
authoritative full history once, outside the write transaction, and owns its
immutable snapshot only for that operation. This is not a process-wide cache.

Graph/Evidence revisions alone cannot justify reuse: direct storage metadata
mutations can bypass application counters. A new optional port reads the
existing catalog epoch/sequence alongside those revisions. PostgreSQL reads it
in the same owned repeatable-read snapshot; in a repository write transaction
it locks the catalog-state row until commit. A lower-level metadata writer
therefore either changes the generation before validation or serializes after
the accepted effect. Epoch replacement, repository/origin mismatch and changed
Graph/Evidence fail closed. Provider authorization, document policy, capture
and original-pin checks, claim fencing and head-read watermarks remain in place.

Only the full authoritative loader in a positively identified read-only
transaction creates a reusable generation. An owned write image, unknown
adapter, or caller-supplied query history never receives that authority.
Unsupported stores retain the slower original path. When metadata changes
during provider I/O, reconciliation defers local bindings/admissions but still
atomically records the safe provider page and matcher cursor; it does not invent
new pre-read evidence or lose page progress. Shared query projection also reuses
the complete history already supplied by its coherent caller instead of issuing
a second full-history read.

Tests establish actual admitted/integrated/pinned effects while rejecting every
application history read made inside a writer. One batch's history reads fell
from two to one. Direct storage history writes without Graph/Evidence changes
invalidate prepared work; late competing owners remain conflicts; historical
source archive behavior and original pins remain unchanged. Broader PG race
coverage passed (application 32.930s, store 19.650s). After restricting reusable
generations to authoritative loading, the final focused race run passed
(5.432s / 3.229s), including standalone admission and caller-history authority
assertions. PostgreSQL-tag vet and fresh temporary API/MCP builds also passed.

The follow-up bounded-index work was separated as follows; the next section
records implementation and verification of worker preparation:

1. Read exact accepted publications and ordinary/carry witnesses using the full
   existing publication key, with deterministic bounded witness selection.
2. Revalidate the exact frozen witness IDs for historical admissions, not the
   first 128 IDs of a later index; late lower IDs must not displace old proof.
3. Check foreign source-SHA ownership repository-wide and read complete branch
   binding dependencies. Never infer absence from a truncated lifecycle list.
4. Page matching pinned publications, including accepted records not yet seen
   by the discovery worker. Keep pre-provider generation fencing for all new
   targeted reads. SQL indexes are lookup aids, never proof authority.

At that checkpoint the full initial index/lifecycle build was still present.
The next increment removes the ordinary-history build from normal PostgreSQL
workers, not the complete lifecycle projection or whole-history query path.
Neither constant-time history queries nor a measured cloud latency improvement
is claimed. Installed services and live #488 are unchanged.

### Targeted admission evidence (2026-10-11)

The PostgreSQL path now prepares admission/application/supplement snapshots from
exact accepted publication IDs and indexed ordinary witnesses instead of loading
every history event. New admissions select the first 128 sorted witnesses per
full publication key. Historical admissions request the exact immutable IDs
stored in their receipt, even if newer lower IDs displace them from that window.
The SQL lookup is not proof: accepted payloads, complete keys and returned IDs
are still validated by the domain/application layer.

All branch-binding events remain in the projection. Birth, rename, archive and
name-reuse dependencies must not be truncated to a recent page or replaced with
a guessed latest event. Only branch refs are read; history retention tags are
not enumerated to resolve current branch destinations. This reduces work with
unrelated conversation/publication history, not with arbitrarily large branch
lifecycle histories.

Provider source heads are unknown before HTTP. The pre-read snapshot therefore
freezes publications, bindings, refs and metadata generation first. After HTTP,
carry witnesses and repository-wide competing SHA ownership are read only in a
coherent snapshot with the same original generation and public revisions.
Hydration copies the prepared evidence; it never mutates the frozen input or
advances its generation. A changed generation suppresses admissions while the
safe provider receipt/matcher progress remains durable. The final write uses
the hydrated snapshot under the existing generation/authorization/lease fences.

Historical query projection still consumes its caller's complete history,
including accepted pinned publications not yet discovered by the worker. That
path is deliberately not replaced with a truncated discovery queue. Independent
review identified a negative foreign-owner lookup that could scan all same-owner
observations. The final adapter uses two branch-identity index seeks (`<` and
`>`), with an EXPLAIN regression over 4,096 same-owner observations. Long branch
names use bounded SHA-256 index keys plus exact string predicates, so the lookup
does not add an oversized B-tree tuple failure to otherwise valid writes.

Validation:

- With 4,355 stored history events, historical contribution preparation loaded
  two events: 1,178 serialized bytes instead of 2,154,905. Local preparation took
  4.633ms in that fixture; this is not a cloud latency or end-user page benchmark.
  The original frozen witness had been displaced beyond the first 128 IDs by
  later arrivals, and the same context/memory contribution still applied once.
- Guards reject every full-history read and retention-tag enumeration in normal
  worker preparation. Focused application tests passed (75.559s); broader app
  race tests passed (53.370s). Separate edge race tests passed (15.215s), covering
  carried-head persistence, stale carry arriving during provider I/O, a foreign
  owner outside the witness window, and late memory with displaced witnesses.
- Adapter race tests passed (5.808s), including exact-key/ID validation, coherent
  reads, document authorization, cancellation, complete bindings, legacy archive
  parity and index plans. A concurrently started broad run compiled an earlier
  test fixture with a foreign-repository ref pointing at the wrong snapshot;
  the corrected fixture passed three race runs (2.222s), and the stable broad
  store suite passed (33.158s). No product rule was weakened for that fixture.
- The complete non-PostgreSQL backend suite passed (application 393.034s), normal
  and PostgreSQL-tag vet passed, and fresh API/MCP binaries built successfully.
  Frontend unit suites (26), architecture, i18n and build passed; existing
  duplicate-translation and bundle-size warnings remain. Two actual-process
  PostgreSQL API/MCP browser tests passed (6.3s), including OAuth/revocation and
  REST/MCP status parity for an empty configured test account.
- Real CLI/Git-hook sync E2E passed all 203 assertions against a separate disposable PostgreSQL
  database. This covers original memory inheritance, protected branch identity,
  merge/pull/next capture, historical rewind, chunk transfer and live capture
  without moving shared main. The installed dogfood binaries were not replaced.

The broad CLI command reached its package-wide 10-minute test budget while an
individual test had run for only two seconds, without an assertion failure.
The delivery CLI package's 440 tests subsequently passed in four disjoint groups
of 110, from the correct package directory, with the same per-process timeout
and serial in-process test execution. The result contains 440 unique passing
top-level tests; other CLI packages also passed. This harness timeout is not
reported as a passing single full-suite command or a product timeout. The two
owned browser/sync databases were dropped after successful validation; logs
remain available, and the shared disposable PostgreSQL cluster stays running.

Independent review found no remaining correctness blocker in the changed
boundary. Bounded ordinary-evidence preparation is complete for normal
PostgreSQL admission, context-head, contribution and supplement workers. Entire
branch lifecycle/current refs, compatibility fallbacks, and full query/UI
projections are explicitly not constant-bounded. Installed services and the
real #488 recovery are still pending; test databases and temporary binaries do
not count as a production or local dogfood rollout.

### Request-local query indexes (2026-10-11)

The initial repository/graph response still needs complete committed metadata
to describe its topology. Conversation documents are not loaded by that query.
The avoidable cost was repeated processing of that same metadata: once per
branch selection, once per PR verifier, and again for branch-memory projection.
Replacing history with the worker's discovery queue would be incorrect: accepted
late memory publications can exist before that queue has seen them.

The query path now prepares branch history and snapshot lookup tables once in
the caller's pinned read. Target associations, original receipt order, completion
IDs, branch births and publication provenance share that request-local input.
Branch selection still performs its own Git ancestry and inclusion checks; a
completed integration remains historically acknowledged after a reset without
reintroducing its excluded context. No index is persisted or shared between
requests, and the index cannot authorize writes.

Branch-memory projection reuses the same verified contributions and history
instead of reading and verifying them again. Empty Git contribution results are
cached for the same request too. Verification caches reject a foreign repository
and invalidate when given a different immutable history slice, including an
equal-length replacement. Mutating the same input slice in place is outside this
internal immutable-input contract; no mutable caller history is globally cached.
Read failures and canceled requests do not become successful cached results.

Historical Git contribution verification now loads only historical inputs from
the supplied history and stored proof records. It no longer reads current refs,
admission revisions or competing-owner maps as if it were preparing a new write.
Full publication/binding validation and the original frozen witness IDs remain
required. Current ownership checks and generation fences still apply to actual
admission and application writes.

The publication evidence index also retains one witness slice per exact
observation key rather than copying it for every matching publication. Public
accessors still return defensive copies, and historical witnesses are never
truncated to the current worker selection window.

PR participation queries likewise build one validated immutable history index
per repository read, then qualify each checkpoint's canonical/alias candidates.
All history is validated before filtering, including unrelated invalid input.
Merge-time cutoffs, attachment timing, cross-worktree aliases, reused identities
and conflicting SHA owners remain checkpoint-specific. The implementation uses
the same verifier for workers and queries; it does not add a second acceptance
policy. Canonical source branches avoid allocating witness/attachment copies
that the existing matching rule does not need.

Local synthetic measurements, not end-user or cloud latency promises:

- Selecting 128 branches across 65,536 history events: median 115.49ms to
  12.98ms; allocation 37.01MB to 22.10MB per projection. Both measurements include
  index construction; the baseline uses the previous resolver through a Go
  test overlay without changing the development source. A smaller 32-branch,
  8,192-event case improves from 4.06ms to 1.04ms.
- 1,024 publications sharing 4,096 ordinary witnesses: publication-index
  allocation 70.16MB to 2.86MB, median construction 9.34ms to 5.33ms. This removes
  multiplicative copying, not the necessary cost of returning requested witness
  lists.
- Building verifiers for 64 PR checkpoints with 4,096 unrelated observations: median
  172.02ms to 1.60ms; allocation 280.95MB to 2.83MB. A canonical repeated-key
  case with 257 witnesses/publications improves from 2.59ms/25.63MB to
  0.259ms/0.578MB. The equivalent alias-heavy case still needs alias/witness
  matching and remains about 26MB; it is not claimed to be constant-cost.

Validation for this increment:

- The application suite's 412 entries run in four disjoint 103-test groups:
  411 pass and one deliberately skips outside its isolated publication E2E
  harness. That helper runs in the successful real-Git E2E below. Each expected
  top-level entry is accounted for exactly once. All other backend packages pass
  on the final source; normal and PostgreSQL-tag vet pass. The initial whole
  backend run failed seven subcases whose hand-built ordinary observations
  omitted repository identity. Their fixtures now provide that identity rather
  than weakening the repository check. A separate manually primed memory-cache
  fixture now binds its input before setting verified results.
- The combined PostgreSQL/application/domain race regression passes (app
  150.318s, domain 1.742s), including historical acknowledgement, reset selection,
  exact old witnesses, late memory, and generation fencing. Concurrent-writer
  graph/pending/position and context-segment checks pass separately (2.401s).
  Guards forbid redundant history/ref/revision reads during historical proof
  verification and require one history/ledger read for branch memory.
- Independent edge tests reuse one evidence object across complete, missing
  publication, missing ordinary witness and restored histories. They preserve
  acknowledgement, actual context inclusion and the exact original memory pin.
  Cancellation, duplicate completion first-occurrence semantics, reused branch
  names and causal code selection also pass with the race detector (8.593s).
- Two actual-process PostgreSQL API/MCP browser tests pass (9.0s), including
  OAuth/PKCE/revocation and read-only REST/MCP status parity. Real CLI/Git
  publication E2E passes 27 assertions: queued PR before source arrival, squash
  finalization, automatic completion, pull selection and the next native capture
  continuing the promoted context. Both owned test databases were dropped.
- Independent review found no actionable correctness blocker. The public-tree
  preflight and secret scan pass; formatting and diff whitespace checks pass.

These results close the repeated-computation increment, not a constant-time
initial graph query. Full history, contribution/proof collections and relevant
alias matches still have proportional cost; indexed pagination of every read
collection is not implemented here. No customer records were edited, no new
GitHub evidence was fabricated, and installed services were not replaced. The
changes remain uncommitted in the development worktree; PR/CI, local installation
and real #488 recovery remain rollout gates. The latter still requires a valid
GitHub App connection, not operator database repair or a personal-token bypass.

### Release preparation (2026-10-11)

The cumulative implementation is being packaged on `codex/494-automatic-context`
from the unchanged `main` baseline. Final web unit, dependency-direction, locale
and production-build checks pass. The PostgreSQL store suite passes against a
disposable PostgreSQL 16 database (64.217s), with no customer records changed.

The expanded PostgreSQL application suite now has 175 `TestPG` entries. CI must
not run all of them three times under one aggregate 120-second package timeout.
The deterministic runner compiles once, discovers all matching top-level tests,
and runs serial batches of at most five from the actual package directory. It
retains the race detector, three repetitions and each batch's 120-second bound;
empty/duplicate discovery, child failure and cancellation are errors. CLI
delivery tests use the same batching because their aggregate run previously
exhausted the default package budget. Other CLI packages remain in the normal
test command. The populated PostgreSQL REST/MCP contribution-parity regression
is explicitly included in CI, not just the existing empty-repository check.

Migrations 0079–0091 are additive and use the existing serialized transactional
migration runner. Migration 0091 creates ordinary indexes on existing history;
it is not a zero-downtime operation for a large database. Local installation must
preserve a database dump, binary/config rollback copies and existing credentials,
stop only the API/MCP services, apply migrations before serving, then verify
readiness and authenticated query contracts. Never change the transactional SQL
to `CREATE INDEX CONCURRENTLY` or restart the legacy FS service over PostgreSQL.

PR/CI, installed-service validation and provider-backed #488 recovery remain
separate gates. The local API has no GitHub App credentials configured. A green
test suite cannot establish that real historical recovery has completed.

### Evidence and rollout gates

Use synthetic fixtures for committed tests. PostgreSQL concurrency/rollback
tests are required; FS adapter replay tests cannot establish PostgreSQL ACID.
Compare API and MCP results at the same repository revision. Record measured
latency and bounded provider work without increasing defensive timeouts.

Historical #488 recovery must run through the same verified automatic path used
by customers, not direct data edits. Missing legacy proof remains explicitly
unresolved until that path obtains it. Deployment and paid provider evaluation
remain outside this work.

## References

- [Git fetch](https://git-scm.com/docs/git-fetch): fetched refs/objects and forced
  update reporting, not a remote operation journal.
- [Git reflog](https://git-scm.com/docs/git-reflog): changes observed locally.
- [GitHub webhook redelivery](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/redelivering-webhooks): do not rely on automatic failed-delivery replay.
- [GitHub PR GraphQL objects](https://docs.github.com/en/graphql/reference/pulls):
  force-push events expose before/after commits; partial or missing evidence must
  remain partial, not be interpreted as an empty complete history.
