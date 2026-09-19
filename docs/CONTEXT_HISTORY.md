# Context branches, working positions, and retained history

## Shared query semantics

`GET /repos/{repoID}/context-query` and remote MCP history/list/search scopes
use the same application query and domain selection rules. PostgreSQL reads
repository defaults, selected position, snapshots, refs, history and revision
inside one repeatable-read transaction. The FS development adapter does not
provide this multi-object isolation guarantee.

`all` includes stored pending captures. `current` follows natural and active
overlay ancestors of an explicit position. `previous` returns retained records
outside that ancestry; it does not guess temporal order from timestamps.
`archived` includes logical archived identities even after their names are reused.
Selecting a past position never moves shared refs or deletes later records.

The full repository view includes versioned `semantics.merges`. Completion,
current placement, source availability and conversation lineage are separate
fields computed before any UI folding. Web uses these server facts exclusively
when present; a compatibility reader remains for servers predating this field
during rolling upgrades. MCP history pages expose the corresponding facts too.
Snapshot scope does not revoke historical receipts. Branch-filtered queries
retain the birth records needed to interpret their completion facts.

Explicit code revert/reapplication and provenance-aware memory validity are
subsequent parts of issue #208; current placement is not a substitute for them.

## Completion and live-read contracts

Local sync reserves advertised objects until its complete push/pull operation
finishes. Automatic capture collection takes an exclusive OS lock; active
readers cause collection to defer instead of deleting an advertised document.
Deferred capture pairs are stored under `.cxt/capture-collection` and retried
by later captures after rechecking provider, session, complete-prefix and
reachability evidence. Deferral survives a process restart. Readers may run
concurrently, and the kernel releases their reservations on process exit.
Document publication and chunk repacking participate in the same boundary.
This coordinates current CLI processes; it is not a multi-file ACID claim,
and old CLI processes must be restarted to participate in the new protocol.

A server-issued PR completion is an immutable acknowledgement. Later same-branch
reordering may supersede its graft placement without revoking that operation.
The graph retains the completed operation and distinguishes historical placement
from current conversation ancestry; it must not redirect current refs/children
through a superseded placement or reconstruct a cycle.

`RepositoryView` owns projected branch memberships. `PendingView` supplies raw
capture patches at a matching graph revision and intentionally omits `branches`;
clients preserve that field from the full generation. Stored memory is fetched
and cached by immutable `memory_hash`, not by the mutable snapshot attachment.

Revert/applicability events and the common server-owned Web/MCP read model are
tracked in #208; this contract fix does not claim those follow-up stages ship.

Status: accepted product behavior implemented and integration-tested on 2026-09-16.
The checklist below records the implemented contract and its verification. This
document supersedes the earlier proposal to repair every shared-tip alias by
inserting synthetic birth and merge snapshots.

## Product contract

1. A newly created logical Git branch has a durable context branch identity
   and creation record. Keeping the provider session open must not skip that
   record. Creating a branch without checking it out does not change the
   current working context or provider session.
2. Collaborators checking out the same remote logical branch share its context
   history. Authors and provider sessions remain distinct. A local binding to
   an existing remote branch is not another birth of that remote branch.
3. `--orphan` starts a context root without conversation ancestry. It inherits
   project memory with explicit provenance; the old conversation stays stored.
4. Moving code back to an earlier commit selects the context associated with
   that code position. The current conversation and memory projection must not
   silently substitute the later branch tip for that position.
5. Earlier progress remains retained on the server and discoverable through
   read-only MCP and the web UI. Selection, graph visibility, archival, and
   deletion are separate operations.
6. Retained progress is collapsed in the graph by default. A visible count
   opens the group; it can be collapsed again. These actions are local display
   state and do not move refs, dismiss sessions, or delete server data.
7. Historical reconstruction uses verified records only. Missing edges stay
   unknown; shared snapshot hashes alone do not prove a branch birth or merge.
8. Failure to durably record a branch creation locally must reject the managed
   Git creation before its ref update commits. Network, server, and provider
   failures leave a retryable operation and do not gate Git on connectivity.

Chunk deduplication reduces storage and transfer cost. Commit ancestry and
server-accepted operations establish ordering. Neither chunking nor a per-call
prompt budget guarantees that repeated context handoffs consume no tokens.

## Separate the identities

| Identity | Meaning |
| --- | --- |
| Repository | Authorized cloud repository and its working replicas |
| Context branch | One logical branch, stable across rename and collaborators |
| Local branch binding | A Git ref in a particular replica bound to a context branch |
| Working position | The code/context selected by one worktree and supported app session |
| Provider session | A native Codex/Claude conversation; not a branch identity |
| Snapshot | Immutable captured conversation content |
| History event | Verified creation, attachment, move, archive, or merge with provenance |

Two local worktrees can select different snapshots on the same context branch.
Moving one working position must not rewind a teammate's position or silently
force the shared branch ref. New captures use the selected source context;
synchronization preserves concurrent shared history.

Branch names and independent local generation counters are not sufficient
identity. Persist operation IDs before execution, reuse them after interruption,
and deduplicate accepted operations on the server. Interpret remote bindings
using repository identity, branch identity, and upstream mapping: `--track`
implies `-c` in Git, so those flags cannot define opposite identity policies.

## Graph and history browser

The server transaction and consistent graph-read guarantees are specified in
[Collaboration transactions](COLLABORATION_TRANSACTIONS.md).

Workspace tabs use `/<namespace>/<workspace>?tab=members`, `?tab=connections`,
`?tab=settings`, and `?tab=onhold`. A named repository uses
`/<namespace>/<workspace>/<repository>?tab=onhold`; the context tab uses the
repository URL without a tab query. Repository names such as `settings` remain
valid because tab selection is separate from the path. The former `/-/` tab
URLs render a not-found screen and do not redirect or load workspace views.
Tab URLs do not grant access; the backend's existing role checks remain the
authorization boundary.

For existing progress `A -> B -> C`, selecting code/context A and later saving
D produces two real paths:

```text
A
|-- B -- C   previous progress (retained)
`-- D        current work
```

The default graph shows the current path and an expandable retained-progress
count. Expanded groups retain real parent edges and their original branch
identity. They are not relabeled as new Git branches. Active paths and shared
ancestors remain visible when a historical group is collapsed. An explicitly
selected historical snapshot must be revealable; collapsing it returns graph
selection to an appropriate visible snapshot without changing the live agent.

The context browser should expose Current work, Previous progress, and All
history. A history group records its branch, before/after positions, actor,
time, snapshots/sessions, and server-confirmed storage status. Viewing,
comparison, and starting a new branch from that code/context position are
separate actions. Code and context must be selected together for an actual
working-position change.

Keep these dimensions independent:

- Current vs previous progress: relative to the selected working position.
- Active vs archived branch: branch lifecycle, including repeated names.
- Pushed vs unpushed vs uncommitted: synchronization/commit state.
- Expanded vs collapsed: graph display state only.

### Graph activation and branch evidence

Activating a snapshot, branch creation, or PR merge reveals the corresponding
context viewer in the center pane. Repeating the same selection also reveals
it. Loading the selected document may require a second scroll adjustment;
manual reading cancels that adjustment. Background refreshes never move the
reader. A creation/merge operation can reference an existing snapshot, so its
selected marker and viewer explanation remain distinct from snapshot identity.

The PR merge records panel separates two independent facts:

- **Merge completion:** a server completion and available ancestry prove that
  both the source and prior destination remain in the resulting history.
- **Conversation lineage:** the source may reach its recorded creation point
  through natural parents, reuse that exact snapshot, reach it only through
  an append, or have no demonstrated path to it. Missing snapshots or an absent
  creation identity make the relationship unknown. An explicit orphan birth
  proves a request to start a new conversation root.

Legacy destructive graft seams in `parents[0]` count as append edges for this
classification. If natural ancestry is incomplete, an available append path
does not establish that it is the only path to the creation point.

A completed PR does not prove that every development turn was captured. An
unchanged source is not evidence of missing turns either. An append can preserve
earlier main without proving that the incoming conversation began there. The
viewer must report those limitations instead of inferring either an intentional
rewind or a new conversation-parent edge. Stored parents and memory remain intact.

Several PRs may complete at the same destination snapshot without moving its
ref. Their projected operation nodes form a continuous chain; later main
captures connect to the latest operation already recorded when they were
created. Retained captures from before a later completion keep their earlier
operation association after a rewind. These are verified
operation markers, not newly persisted conversation snapshots.

The first implementation adds a Previous progress panel alongside the existing
archived-branches panel. It uses server branch ref movement records and the
currently available snapshots to find paths outside the current branch ancestry.
Each group shows the recorded branch, before/after tips, time, count, and controls
to expand, collapse, or view its tip. Shared ancestors and other active paths
stay visible. Previously published hook/stash snapshots stay in history rather
than becoming unpushed or uncommitted again.

This panel is a projection of existing evidence, not the complete working-position
model. It cannot identify an unsynchronized local rewind, the actor/cause of a
legacy move, or missing branch identities. Missing ancestry remains visible;
no synthetic edges are created. Ref logs alone do not replace durable server
retention roots. Display state resets when changing repositories or remounting
the graph; folding does not dismiss a pending session or change a ref.

## Read-only MCP access

Retained data must be discoverable without knowing its hash beforehand:

- List current, previous, archived, or all history with bounded cursor pages.
- Fetch older event ranges, not only the last readable messages.
- Search with branch/history scope and return the snapshot, code position,
  provenance, and relationship to the selected working position.
- Load memory pinned to that position/version. Later memory is available by an
  explicit historical query, not silently substituted during a rewind.
- Carry an explicit working-position/snapshot selector from the client. A cloud
  MCP server cannot infer a caller's local Git HEAD from a repository name.
- Preserve repository authorization on every page and referenced object.

Remote MCP now exposes cursors for all six read-only tools, complete archived
event fragments, and exact memory object fragments. `current` and `previous`
scopes require an explicit position. See [MCP](MCP.md) for the response and
continuation contract. Hash-pinned history is verified; unknown pre-upgrade
memory versions are not reconstructed from timestamps.

## Persistence and failure boundaries

Record a small local operation before a managed Git ref change. It includes
the repository/binding identity, operation ID, observed Git before/after values,
and known context sources. Provider materialization and server upload are
separate retryable stages. Do not infer success from a ref file existing or
from waiting 1.5 seconds.

Post-checkout runs after Git changes the worktree and cannot roll that change
back by returning an error. Creation gating belongs in a supported pre-commit
ref-transaction phase, with separate handling for symbolic/unborn HEAD updates.
Preserve pre-existing user hook exit statuses. Reconcile interrupted operations
against observed Git state rather than treating every prepared event as a
successful birth.

Protect the previous tip and its referenced snapshots, memory, and chunks with
durable retention roots. A server-confirmed history transition must not expose
a current ref without the corresponding retained history. Local-only data is
reported as pending synchronization until the server confirms it.

Treat immutable object staging separately from publishing refs: an unreachable
staged object is not itself a failed-history event, but partial publication
must have a deterministic retry path. A retry must not generate a new UUID or
timestamp for the same logical creation/merge.

The CLI replica is not a trusted proof of intent. Preserve damaged material,
validate object hashes and source relationships, and recover from authorized
server copies where available. A second directory on the same disk helps with
partial local damage, not disk loss or a user bypassing hooks. Unsynchronized
data without another copy cannot be promised recoverable. Device signing is
not a prerequisite for these correctness fixes.

## Model and compatibility

Store branch/history operations as explicit domain records and render them as
first-class graph information. They need not be provider conversation events.
The current invariant remains `Snapshot.ID == DocHash`: changing snapshot
metadata alone does not create new captured content.

If implementation uses a dedicated snapshot kind for an operation, it must
define restoration, memory traversal, statistics, schema negotiation, and
legacy-client handling before publication. Do not fabricate a provider session
ID or duplicate conversation statistics merely to draw another node.

A merge must identify the actual source snapshot, target state, PR/repository
identity, and Git code revision. A delayed webhook must not select an unrelated
new tip solely because the branch name was reused. Preserve main's visible
continuation and the source branch's identity without rewriting natural parents.
Deduplicate repeat deliveries of the same merge.

## Historical recovery

Keep existing snapshots, natural parents, raw conversation, lifecycle tags,
and ref logs. Use the evidence available to display known activation/archive
facts. Reconstruct an edge only when Git/PR/capture records support it, record
the recovery time and evidence, and do not reactivate an archived branch as a
side effect of making its history visible. No blanket synthetic birth/merge
migration is authorized by this design.

## Implementation and acceptance checklist

- [x] Record accepted behavior and correct documentation overclaims.
- [x] Preserve existing rejecting Git hooks; verify prepared and pre-push failures.
  Post-operation failures retain the user hook status while still observing
  completed/aborted Git operations. The cxt prepared vote now gates durable
  creation recording; connectivity is handled by replay after Git commits.
- [x] Record/replay committed branch creation independently of provider switching.
  Prepared-only operations require an exact Git witness; an unproven orphan
  remains pending and is explicitly reported rather than fabricated.
- [x] Separate worktree working positions and resolve recorded source context.
  Code positions without verified associations report an error and preserve the
  existing selection; older missing associations are not guessed.
- [x] Retain earlier progress durably across rewind and interrupted synchronization.
- [x] Default-collapse previous-progress groups evidenced by server ref movements;
  independently expand/collapse them and reveal a selected historical tip.
- [x] Preserve shared ancestors and other active branches while folding history.
- [x] Add previous-progress browsing and bounded MCP pagination/event ranges.
- [x] Bind remote MCP current/previous reads to an explicit position.
- [x] Pin recorded historical memory state, including explicitly empty memory,
  and preserve the owning snapshot when an orphan inherits ancestor memory.
- [x] Implement read-only `doctor` and `branch operations`, plus verified
  `branch replay`, before documenting these commands as available.
- [x] Implement verified object recovery from a server copy, preserving damaged
  local evidence. `repair --from-server` now restores verified cloud objects in place with durable quarantine; local-only damage remains explicitly unresolved.
- [x] Verify actual Git operations, two worktrees, repeated names, provider failures,
  offline replay, duplicate webhook delivery, and browser keyboard interactions.

Update each item only when its implementation and relevant checks are complete.

### Implementation order after the first slice

1. Durable branch creation records and replay: prepare/commit/abort, creation
   without checkout, tracking attachments, unborn/orphan HEAD, and interrupted
   operations. Keep provider handoff separate from recording the event.
2. Per-worktree working positions and code/context resolution, including reset,
   explicit start points, and pinned memory selection.
3. Server-confirmed retention/history events, stable branch identity across
   collaborators and reused names, and deduplicated PR merge bindings.
4. Previous-progress browsing and remote MCP cursor/event-range access using
   explicit positions; inspection and recovery tools with precise errors.

First-slice verification: CLI unit/integration suite; frontend unit suite;
browser fixtures covering independent groups, keyboard controls, tip selection,
teammate paths, historical hook/stash labels, and unchanged archive behavior;
web production build and i18n key checks. Real backend OAuth/profile browser
cases are separate full-stack checks and are not evidence for rewind retention.

### Current implementation evidence

- A Git-directory journal records creation votes before Git commits and replays
  operations with a durable committed callback. A later same-name/same-OID
  reflog entry cannot identify which transaction completed; prepared-only
  operations remain unconfirmed. Empty replay cannot bind a repository
  before remote setup, and a reused/mismatched repository identity is rejected.
- Available active provider bytes are checkpointed before a current-code birth.
  Provider capture failure preserves the verified saved baseline and does not
  terminate a conversation. Ordinary symbolic HEAD changes are not orphan births.
- Worktree positions use separate files and a recoverable save transaction.
  Historical parent selection excludes future grafts; memory remains pinned
  even when the snapshot's mutable memory attachment later changes.
- Server history records and immutable retention tags protect previous tips.
  PostgreSQL publishes retention and the continuation ref with one transaction;
  the development filesystem store uses a recoverable journal. Duplicate
  acknowledgement never reapplies an old transition to a newer branch tip.
- The web graph can browse an explicitly chosen server-recorded position and
  show branch birth/attachment evidence independently of conversation hashes.
  Browser fixtures assert that these actions issue no mutation requests.

Verification on 2026-09-15, using isolated binaries and repositories:

- CLI and backend full `go test ./...` and `go vet ./...` passed.
- CLI storage, application, and command packages passed `go test -race`.
- Sync E2E A–L passed, including two real worktrees: reset one from B to A,
  save C with parent A and no future B graft, publish retained B/current C to
  the server, and leave the other worktree at B.
- PostgreSQL smoke passed against a new database and all migrations. The
  filesystem and PostgreSQL tests verify that protected-branch policy rejects
  new history advances while acknowledging an already accepted event.
- The 16 browser fixtures passed. Both real-backend cases passed separately:
  public profile rendering and OAuth/PKCE/MCP read/revocation.
- Replay concurrency regression passed: a blocked remote lookup does not hold
  the Git creation journal lock, and delayed replay cannot reset a newer local
  position to its old birth target.

### Additional implementation evidence (2026-09-16)

- Branch identity is causal and independent of names and client clocks. Birth,
  rename, archive, restoration, tracking attachment, position, and continuation
  carry logical identity. Concurrent same-name births conflict atomically in
  both server stores; a continuation cannot use a different identity even when
  its source snapshot matches the current ref.
- Local tracking aliases resolve to the canonical server branch through capture,
  push, load, checkout, memory selection, and rename. Removing/renaming a local
  tracking branch changes only its local binding. Its server branch is retained.
- Renaming preserves every worktree's selected snapshot, code, and pinned memory.
  Restoring a known archived branch creates a new identity and preserves the old
  identity's archived state. UI/MCP browsing uses those identities across rename
  and name reuse; raw conversation labels and timestamps remain unchanged.
- A continuation from an unpushed prior tip publishes its source as a normal
  fast-forward prerequisite before retention CAS. Divergent server work blocks
  publication and remains untouched. Acknowledged retries do not rewind refs.
- `cxt doctor [--json]` and `cxt branch operations [--json]` are read-only and
  run before normal adapter initialization. They remain usable when `.cxt` is
  missing/damaged and do not imply server acknowledgement. `branch replay`
  retries only evidenced operations and reports those that need Git evidence.
- Branch-history ordering uses an explicit dependency graph and a priority queue;
  it does not repeatedly shift or rescan the entire remaining event sequence.
- PR promotion uses repository + PR number + full head/merge Git revisions to
  resolve historical context identity and snapshot. A server receipt and retained
  roots precede append; webhook and CLI use the same service. Renames/reuse do not
  change the bound source. Missing/ambiguous evidence blocks promotion; legacy
  abbreviated Git links alone do not prove an exact historical source.
- `branch recover <id> --confirm-orphan` records explicit confirmation only in
  the recorded worktree while the exact HEAD remains unborn. It preserves the
  distinction between user evidence and a recovered committed callback.
- `repair --from-server` preflights an isolated cloud replica, quarantines every
  replaced predecessor, restores verified objects in place, and keeps healthy
  local-only refs/objects/positions. It never infers missing data or damage intent.

### Compatibility rollout and final verification (2026-09-16)

Repository settings expose **Branch history protection**. Update every CLI and
sync each replica before enabling it. The maintainer-only
`POST /repos/{repoID}/context-protocol` transition is idempotent and irreversible:

- Version 0 remains compatible with legacy writers. Existing repositories are
  not silently upgraded by installation, registration, or a regular push.
- Version 1 requires a stable `branch_id` for branch ref writes and joins. Birth,
  rename and archive events project physical refs inside the graph transaction.
  Name-only writes and newly minted legacy lifecycle tags are rejected.
- Migration assigns IDs only to confirmed live pointers. Ambiguous reused names
  block the transition. Legacy replicas may adopt the first proven server
  identity without moving local snapshots, code positions or pinned memory;
  this exception never applies to a released/reused name.
- Filesystem branch refs support identity-bearing JSON as well as legacy hash
  files. PostgreSQL migration `0040_context_protocol.sql` adds the corresponding
  repository version and ref identity columns. The CLI directory remains a
  replica; production storage is PostgreSQL.
- Diverged append commits overlay changes and the ref in one transaction. A
  losing append cannot leave an overlay behind. Filesystem recovery also rejects
  a journal whose branch identity changed.

Final verification includes both Go modules' complete test/vet suites, race
checks for CLI storage/application/commands and backend storage/application,
real PostgreSQL migrations and protocol scenarios, web unit/i18n/build checks,
and 20 browser cases including two real-backend OAuth/profile cases. Sync E2E
A–O covers actual Git hooks, worktree rewind, tracking aliases, provider handoff,
protocol activation/name reuse, and damaged-replica recovery with local-only
work preserved. Duplicate first PR deliveries freeze one exact source receipt.

An unknown historical edge or unsynchronized object without another copy remains
unknown/unrecoverable. Diagnostics expose that limitation rather than inventing
an ancestry edge, branch identity, Git witness, or replacement conversation.

### Branch graph event projection

The graph includes separate event nodes for recorded branch births and proven
joins, even when a birth shares its source snapshot hash. A completed PR receipt
proves a join when its source and previous base are reachable from its resulting
base; a ref movement is not required. Without a receipt, a join requires both
an existing append edge and an unambiguous source branch. A pending receipt,
shared current hash, or ordinary fast-forward alone does not prove a merge.
An unbound ref movement publishing the destination branch's own capture is
not a reverse merge merely because main later points to the same content hash.

At a join, main continues along its previous main history; the source branch
remains on a side lane between its recorded birth and join. These event nodes
are a read projection: clicks open the archived conversation at that event;
no synthetic snapshot or rewritten parent is stored. Rename and name reuse
retain distinct birth identities. Orphan branches begin without conversation
parents. An unborn orphan without a capture remains in the operations list.

When the conversation path does not connect a birth to its completed PR, a
separate long-dashed operation edge may connect them. This requires one unique
birth and a verified completion bound to the same branch identity, with birth
preceding completion. The legend and endpoint tooltips distinguish this from
conversation ancestry; no missing parent is inferred or stored. A no-op PR can
therefore retain its own branch lane even when several PRs share one source
hash. Ambiguous identities, pending receipts and cyclic evidence never create
this operation edge. Status dividers extend every passing lane, including its
dash style, instead of inserting an unconnected gap between graph rows.

Branch paths use identity-bound `advance` and finalized `publish` targets, plus
verified completed receipts' `source` and `source_branch_id`. Ordinary automatic
capture need not emit an `advance`, and archived refs are not required. Worktree
`position` selections alone do not prove ownership. Birth insertion follows
existing natural ancestry; it never invents a missing edge or uses a completion's
potentially newer base `target` as the source branch. If multiple identities claim
the same content-addressed edge, leave it unchanged instead of choosing by input
order. These rules also apply to root-only orphan captures.

Existing archive and previous-progress controls still apply to conversation
rows. Proven operation nodes are distinguishable from snapshots and do not
inflate pushed/unpushed/uncommitted counts. Unknown legacy births/joins stay
unknown; the UI does not fabricate them from conversation labels.


### Completed PR joins without a ref move

A PR source binding is recorded before promotion and is not proof of success.
After the server verifies that the source is reachable from the base (including
an already-contained or same-snapshot source), it records a separate immutable
`pr-merge` event with `pr_completed: true`. Its `source` is the bound PR context,
`shared_target` is the observed base before the successful attempt, and `target`
is the resulting base context. The operation ID derives from the binding ID;
repeated deliveries publish one completion. A failed completion write is retried
without rewriting the binding, archive, or ref.

The graph uses this server completion to draw a join even when no ref moved.
It deduplicates a matching reflog join and retains the source identity's birth
and lane. Pending bindings alone still never draw completed joins. Deploy the
matching CLI with the server so pulled history preserves completion metadata.
Older bindings acquire completion records when verified PR promotion is replayed;
the renderer does not infer completion from an old binding alone.

PR source resolution also recognizes tracking aliases. The observation must
match the PR's exact Git head and native `local_branch`, with a prior attachment
of the same worktree to the same context identity. This retains the canonical
shared branch even if the native alias or canonical branch was renamed. A
missing attachment, mismatched worktree, or competing source identity does not
authorize a guessed source. Current refs are never a substitute for this
recorded association.

Integrity verification is scoped to one operation. Each distinct owned
snapshot/document pair is fully reconstructed and hash-checked once, then reused
for duplicate fields and the promotion's completion receipt. Failed checks are
never reused, metadata ownership is still read for each reference, and separate
requests start a fresh verification set. This avoids repeated large-archive work without trusting a process-wide cache
or increasing client timeouts.

## Durable PR delivery

Verified webhooks and authenticated repository requests persist a job before
source lookup. Jobs freeze the PR tuple, Git origin, and base branch identity.
Missing exact source history remains waiting; identity/integrity failures require
attention. Retries never substitute the current head of a same-named branch.

PostgreSQL workers claim the oldest unfinished job per repository using row
locks and leases. Execution is bounded to 90 seconds with a two-minute lease;
expired claims recover after restart, and claim versions fence stale completion
writes. Retry delay grows to 256 seconds. Temporary failures stop after 20 attempts;
late-source waiting remains eligible until the source arrives. An earlier waiting
job holds later jobs in that repository to preserve acceptance order.

Processing is at least once. Immutable source/completion receipts and existing ref
CAS make repeated execution idempotent. Delivery status is separate from graph
history: queued requests never fabricate a completed merge edge.

Git hooks also persist the incoming Git commit range before PR discovery.
Ranges are retained across ORIG_HEAD changes and processed in batches of 200,
up to four batches per invocation, without dropping older commits.

The CLI records an exact PR request before network delivery, and push/pull resend
unaccepted requests. Server acceptance releases local delivery responsibility;
subsequent pulls read the resulting server history. The compatibility synchronous
promotion endpoint also persists first. Viewer access may inspect the latest 100
jobs; member access may submit or retry. The UI shows waiting, retrying, processing,
completed, and attention states with safe reason codes.

## Git rewrite recovery

`post-rewrite` preserves exact full Git object mappings in immutable batches under
`.cxt/worktrees/<id>/rewrite-journal/`, bound to the observing branch identity.
The legacy repository-wide rewrite map is not authoritative for server source
bindings. Context history
now also publishes immutable `position` observations for those mappings, retaining
the original branch identity, worktree, snapshot, and pinned memory. This path
runs while rebase is finishing, independently of provider capture and the normal
position selector's in-progress guard. `git push` and `cxt push` replay it before
syncing history to the server, so PR resolution can find the rewritten head.

Replay never replaces the current branch tip or fabricates a capture. Original
events remain intact. Short hashes, cyclic mappings, unrelated worktrees, and
reused branch identities are not accepted as equivalent source evidence. An
already-recorded observation at the new revision takes precedence over a delayed
replay. Historical recovery of pre-journal data requires independent evidence from the
affected worktree; selecting
an arbitrary recent session is not recovery.

### PR #164 incident recovery (2026-09-16)

The original explanation, “no source association exists,” was incomplete. The
source worktree recorded context at Git `79609b5bbbe282b612daa60bde0ac5423a5495c8`,
and the Git rewrite journal mapped it to PR head
`d1e75ca6d69934f742038adccd080082ea12bc95`. That mapping was not published in
server history. The selected context was an inherited snapshot; the new
development conversation ran in the primary worktree and was not captured by
the source worktree's cwd-scoped provider discovery.

Recovery used the retained native session prefix through the recorded commit
invocation at `2026-09-16T09:19:14.038Z`. The tool invocation names the exact source
worktree and Git commit command; the old-to-new revision is independently present
in the Git reflog and rewrite journal. No later conversation was imported.
The resulting archived snapshot is
`sha256:e7233fc24a1c07064043bc9b964ae4e58472af7459f26fd02dc316dc764f2208`.
Its creation and association dates are the actual recovery time, and its message
identifies the historical cutoff. The original selection remains its parent.

Metadata backups, prefix checksum, recovery result, and the one-off recovery
program are retained locally in `.cxt/recovery/pr164/`; native session data is not
part of the source repository. Recovery adds immutable archive/history records
without changing the live provider session, its pending pointer, or Git refs.

The server accepted the recovered source and completed PR #164 on 2026-09-16.
The original natural parent stayed intact; append retained the previous main
through a graft edge. The live app conversation was never restarted or loaded
from this historical prefix.

For new commits, command capture honors an exact Codex app thread ID across
linked worktrees of the same Git repository. A registered pointer is checked
against the native file; an explicit command ID can also find its native file
by matching internal ID and repository ownership when that pointer is absent or
invalid. An owning cxt wrapper
can likewise resolve its registered Claude/Codex session across those worktrees.
A command with a native thread ID that cannot be resolved does not substitute
a newer sibling session. Automatic background capture and branch-switch ownership remain scoped to one
worktree. No cross-worktree recency fallback is introduced, and native ID/path
checks plus the capture-exclusion ledger still apply.

## Linkage reliability audit (2026-09-16, #175)

The completion criterion is recovery from an interrupted operation, not just a
successful first run. A graph edge must follow durable, exact Git/context
observations. Timestamps, current branch names, and the most recent unrelated
provider file cannot substitute for missing provenance.

Rewrite replay now drains every worktree journal in the shared replica, retaining
the original repository, branch identity and worktree on every observation. It
also flushes journal directory entries to disk. Removing or leaving the original
worktree does not prevent a subsequent push elsewhere from publishing its stored
mappings. A damaged journal stops replay with an error rather than becoming an
empty successful result.

A squash can map several original commits to one Git revision. A previously
published rewrite alias does not count as a new native capture and cannot hide
other aliases after an interruption. All proven source observations are retained;
the server may choose a tip only if its ancestry contains all candidates.
Concurrent writers adopt the first immutable observation only when every field
other than the retry timestamp agrees. An unrelated storage failure still fails
and leaves the durable journal available for the next process.

Regression coverage includes partially published squash mappings, concurrent replay
callers reading the same state, failed history persistence followed by a new
service instance, and replay from a different worktree. These tests do not imply
that a deleted native conversation without another copy can be reconstructed.

### Explicit source finalization

A `position`, branch birth, current branch ref, or `[git ...]` message does not
prove that capture has finished. The CLI publishes a separate immutable
`publish` observation only for a completed capture pass. It binds the exact Git
revision, repository, branch identity, worktree and selected source snapshot.
The server requires its matching ordinary source observation before accepting
this finalization. Ordinary history uploads precede finalization regardless of
wall-clock order. Publication never changes the selected project memory.
When a push already contains several completed captures for one source revision,
the verified descendant containing all candidates is published first. This
prevents a worker from binding an older candidate halfway through that upload.
Incomparable candidates require reconciliation before publication.

PR resolution uses finalized sources only. A source still being uploaded remains
`source_context_pending`; it cannot be acknowledged from the first provider or
first rewrite alias to arrive. Squash finalization waits for every original
revision's completion and a single proven descendant containing all candidates.
Unfinalized legacy history is retained and readable, but is not silently upgraded
into proof of a complete capture. Existing completed PR receipts keep their
original meaning. Uncompleted old bindings wait for matching finalization, and a
different finalized source requires explicit reconciliation.

A completed receipt is terminal: replay returns the current base without
re-appending the original source. This preserves a later rewind or continuation,
including when the prior process wrote the completion receipt but failed to mark
its queue job complete. Later captures do not retroactively enlarge an already
completed PR's scope.

### Working position after promotion (#177)

The complete incoming path includes the next capture: Git pull, completed PR
promotion, worktree selection, then context commit. A completed receipt associates
its exact merge Git revision with its frozen **source** snapshot. Its resulting
target may already contain later base work and is not the code-time source.

Git's reference transaction can select an older known context before post-merge
fetches this receipt. After delivery, and again before the next capture, the CLI
can refresh that provisional selection only when all of these remain true:

- The same named worktree, local binding, logical branch and Git revision match.
- Its recorded code move was forward, and the shared baseline has since changed.
- The completed PR belongs to that exact merge revision and branch identity.
- The local shared ref is exactly the completed source, containing the previous
  selected snapshot. A later shared tip is not silently adopted.
- Both the complete old position and the shared ref still match under the local
  mutation lock. A competing capture, memory repin or selection wins its CAS.

Backward resets, explicit same-code selections, detached work, other worktrees
and newer captures are preserved. The operation changes no shared ref or live
provider conversation. Retrying the hook or capturing after an interrupted hook
uses the same checks; it does not require editing `.cxt` data manually.

Project memory comes from pinned ordinary observations for the exact source
snapshot, source branch identity and PR head Git revision. Multiple versions must
have a provable causal memory successor. Conflicting versions remain an error;
timestamps, publication's empty memory fields and mutable snapshot attachments
do not resolve them. Explicitly pinned empty memory remains meaningful.

A source that remains unfinalized for eight attempts moves to visible
`attention / source_finalization_required`, releasing later queued PRs. It is
not marked completed or deleted. Arrival of the matching publication wakes it
automatically; periodic reconciliation also covers a restart or a publication
racing the transition into attention. Other conflicts still require review.

The server verifies the CLI's exact source association; it cannot inspect a
user's private native transcript or establish that a dishonest client supplied
all of it. Missing copies and ambiguous Git transaction completion remain visible
failures, not guessed edges. Upgrade the backend before enabling publication in
the matching CLI, then upgrade every client sharing that repository. Old clients
without source finalization leave new promotions waiting and cannot fetch history
containing this new event kind. Do not roll the backend back to a resolver that
accepts ordinary observations while publication-enabled clients are writing.

### Capture and retry boundaries

Before the first provider save, the CLI durably records a capture pass with its
Git revision, branch identity, worktree, initial context and expected providers.
Each provider outcome is recorded separately. The final source must be one of
this pass's own outputs (or its frozen initial context) and contain every output
in its ancestry. A concurrent pass cannot lend its mutable working position as
proof that this pass finished. Losing an explicitly selected transcript is a
capture failure, not evidence that the provider was unused.

On restart, complete stored outcomes with exact Git observations can finish
publication without reopening a live transcript. A pass interrupted before an
outcome was persisted remains pending and reports its journal path; later
unrelated work cannot prove what that missing outcome was. Other complete
branches can still sync. Corrupt journals fail validation. Rebase's intermediate
detached commits do not capture a fresh conversation onto the default branch;
their completed original observations travel through the rewrite journal.
Intermediate `post-rewrite amend` callbacks during squash are also provisional.
Only a journal explicitly marked by a complete rewrite boundary can finalize
the new Git revision. Legacy journals without this flag preserve associations
but cannot attest completion.

Normal push also retries pending-session pointers after their objects arrive.
Final-session flush waits for an existing capture within its deadline; a failed
flush retains app-session liveness for a later attempt. That registry has a
24-hour lifetime, so this is not an indefinite background recovery guarantee.
Superseded hook data is collected only after provider/session identity and the
entire event prefix are verified; stale, shorter, divergent and referenced
captures remain stored.

Hook identity is validated before liveness registration, capture bookkeeping or
briefing consumption. A claimed session ID paired with another session's native
file is rejected without overwriting the existing pointer. This covers child
task hooks carrying an inherited parent ID. Command-only exact-ID discovery uses
native metadata and the shared Git repository, never a newer sibling transcript;
background discovery still requires its exact worktree registration.

Branch creation replay requires the durable committed transaction callback.
A later reflog entry with the same name and Git hash is insufficient to identify
an earlier prepared operation. Committed operations remain replayable after the
original worktree is moved or removed, using the shared Git repository. Tracking
resolution uses the originating Git admin directory, including `config.worktree`.
If that directory was pruned before a binding was resolved, the operation stays
queued; the replaying worktree's different upstream is not substituted. An
unchanged owner snapshot that is an ancestor of the verified current head keeps
that head and its memory selection as the birth baseline. Failure to inspect Git
is an error, not a successful claim that the branch disappeared.

### Graph state and evidence

Graph projection does not modify stored context or memory. It separates:

- **Branch identity:** rename follows `branch_id`; a reused name cannot take an
  earlier identity's completion. Ambiguous legacy names do not infer joins.
- **Publication:** server branch movements, publication acknowledgements and
  completed PR inclusion preserve the published timeline after refs move.
  Position, attachment, birth and memory-source observations alone do not
  publish a pending capture.
- **Display closure:** pending and unsync roots keep their available natural
  and graft parents, including intermediate hook captures. A missing parent is
  reported rather than represented by a line waiting for a nonexistent row.
- **Retention:** ordinary server tags have a distinct “tagged” status. A tag
  does not attest branch publication or commit a pending capture.
- **Archive versus join:** merely sharing a tip with main does not prove a
  join. An archived branch needs an identity-bound verified PR completion or
  an explicit legacy graft into its originating content to receive that label.

Completed merge rows use the destination identity. A continuation on its
current path connects through the completed operation even if its provider
timestamp predates the server receipt. Explicit subsequent movements away from
that lineage withdraw the active placement. Retained captures without a proven
continuation keep their stored parent edges; timestamps alone do not assign
them to a historical PR.

Branch selection follows a selected identity across polling and rename. Query
loading, errors and successful empty results are separate display states.
Failed reads preserve available content and expose a retry action. Selecting,
folding or retrying a read never writes ancestry.

Uncommitted captures carry a hollow dashed node and a label on their own row.
There is no horizontal uncommitted boundary: topological ordering can interleave
published commits and PR events with pending captures from other sessions.
Pending status does not dim published paths passing through that row.

The pure graph regression fixtures cover rename/name reuse, clock skew, rewind,
pending → unsync → publication, observation-only history, tags and unavailable
parents. Browser regressions also follow SVG endpoints and test polling and
failure recovery. Natural ancestry, stored grafts and display-only lifecycle
edges remain different evidence types.

#### Folding and integrity

The graph validates the complete snapshot input before applying visibility.
Merge evidence, branch operation projection, session boundaries and compaction
markers use that full input. Archive/progress toggles only select projected rows;
they do not rerun ancestry queries or change whether a PR is verified. Visible
rows retain their virtual operation ancestors. A hidden real capture is never
skipped to manufacture a new edge: its folded endpoint is reported separately
from an unavailable parent.

Duplicate snapshot IDs (even identical duplicates) and a cycle in natural or
graft parents stop graph drawing and graph drag operations. The panel reports
bounded ID samples and offers a read retry. The document viewer remains usable.
Validation also covers the full display projection before folding, so hiding an
invalid component cannot hide its error. Cycle diagnostics identify one actual
cycle, not every node blocked by a topological sort. Missing parents remain a
nonfatal, separate partial-data condition. No automatic data repair occurs.

A repository-view index shares ancestry queries across evidence, status and
projection. Iterative traversal supports long chains without recursive stack
limits. DFS intervals prove positive ancestry only; cross edges still require
exact traversal. Cached closures are bounded to 64 roots and 100,000 ID
memberships; larger closures are computed without retention. See
[graph performance](GRAPH_PERFORMANCE.md) for reproducible measurements and their
scope.

A tracking alias can be created in one worktree and committed in another. Its
repository-local Git binding is shared, so PR publication accepts an earlier
attachment for the exact native alias and context branch identity across
worktrees. The ordinary capture proof must still match the publishing worktree,
Git revision, native alias, identity and snapshot. Unrelated aliases in another
worktree do not qualify; same-worktree rename evidence remains supported.

## Live uncommitted capture

Active native sessions use incremental normalized projection checkpoints. On Hold follows durable repository revisions instead of repeatedly fetching the full graph. Capture identity, pending publication and failure/reconnect behavior are specified in [Live capture and repository updates](LIVE_CAPTURE.md).

### Bounded object finalization

Chunk transfer limits the bytes sent over the network; it does not limit how
many cumulative transcripts the server reconstructs in one request. CLI pushes
therefore finalize at most one document and one snapshot per object request,
with natural parents before children. Ref updates are sent only after every
object request succeeds. An interrupted push can leave verified immutable
objects on the server; subsequent negotiation skips those objects and resumes.
A lost acknowledgement never implies that the object was not stored.

This bounds the number of reconstructed documents per request, not the cost of
a single large transcript. Streaming validation/indexing remains a separate
optimization. Other clients can continue to move their refs concurrently;
normal server CAS and transaction rules still govern ref publication.


## Verified Git inverse-change evidence

Git evidence is a separate stream from immutable context branch and PR history.
A completed PR promotion stays completed when a later commit reverses code.
The `/repos/{repoID}/git-changes` member endpoint accepts a target/commit pair
and optional explicit comparison parents. It durably stores the request before
reading the Git provider. Repeated identical submissions return the same job.

The backend compares complete immutable file trees, including file modes,
symlinks and submodule object IDs, and checks target ancestry. Commit titles
are never proof. Renames are represented as exact delete/add paths. Merge
commits need an explicit comparison parent. Exact inversions are `full` or
`partial`; intra-file partial changes, intervening edits and incomplete trees
remain unverified. Unrelated later changes are outside the inversion scope.
A reversal of a reversal is evidence about that reversal commit, not a global
boolean toggle for the original PR.

PostgreSQL migration 0047 persists request state and the terminal result in one
row. Independent pairs can be verified concurrently. Claims use row locks;
lease versions fence expired workers. Result publication and repository revision
advance commit together; provider calls never hold database locks. A restart
recovers expired claims. Transient failures retry with bounded backoff, then
remain available for explicit retry. Terminal results cannot be replaced by
late responses or retry requests. The FS development adapter uses atomic job
files and a process lock; it does not claim multi-process database ACID.

Web shows verification summaries when the evidence section is expanded and
invalidates them on repository revisions. Lists omit large path arrays and use
ID-based continuation; restart pagination to see concurrent insertions earlier
than the cursor. MCP `git_changes` uses the same application query and only a
read port. `change_id` fetches bounded JSON fragments with a generation hash;
changed verification results require restarting the cursor. These are retained
historical facts, not implicit current-branch applicability.

Automatic discovery and file-level code-position applicability are described
below. Provenance-aware memory filtering and active-session briefings remain
follow-up work under #208. Neither discovery nor verification changes the current memory
projection, refs, or the historical PR graph.


## Automatic Git observations and inverse discovery

Accepted CLI history records and PR promotion requests enqueue their full Git
object IDs in the same repository transaction. The CLI's existing durable
history replay therefore also retries observation delivery. Legacy abbreviated
IDs are not guessed. Signed GitHub `push` deliveries retain the delivery ID,
ref, both tips and forced flag before returning success. A delivery ID cannot
be reused with changed content. Both tips are scanned, including the old tip
of a deleted or force-updated ref. Observations never move context refs or a
teammate's working position.

A commit worker reads all explicit parent comparisons, indexes exact
path/content/mode transitions and enqueues every parent. Commit-message wording
is irrelevant: an unlabelled inverse is discoverable. The index supplies
candidates to the existing ancestry and inverse verifier in both directions,
because arrival order does not establish Git ancestry. Reapplication is a
verified inverse of the reversal commit, not a global PR boolean. An unrelated
B between A and A's inverse remains outside A's proof.

PostgreSQL migration 0048 atomically publishes complete comparison records,
ancestor jobs and the scan cursor, with a version fence against expired
workers. Candidate pages publish at most 100 verification jobs with their
cursor. Duplicate deliveries reuse work; a late record discovers inverses
against already indexed records even when their original scan has completed.
Provider I/O is outside transactions. Immutable indexed deltas and bounded
cached ancestry walks reduce repeated provider reads. A shared adapter cooldown
honors GitHub rate-limit responses; workers keep retrying transient discovery
failures with bounded backoff. Integrity failures stay visible for review.

A separate reconciler reads remote branches one page at a time. Each page's
observations and continuation commit together; failures retain the page and
publish a retry status. Reconciliation runs at minute intervals, with five
minutes between complete passes. It observes tips independently and never
infers deletion from a mutable paginated list. It recovers missed deliveries
for commits reachable from a subsequently observed tip. Commits that vanished
before any CLI, webhook or remote observation cannot be reconstructed by
assertion; retained CLI history and explicit verification are additional
sources. No protocol can prove an unobserved, unavailable object existed.

`GET /repos/{repoID}/git-scans` and read-only MCP `git_observations` expose the
same application query: per-commit state, reasons, indexed status and remote
reconciliation progress. Lists are bounded and ID-ordered; restart pagination
for concurrent insertions earlier than a cursor. Web fetches these only when
expanded and invalidates on repository revisions, with no additional polling
timer. Only members can retry through the REST command.

“Completed” means discovery against the index at that pass. Ancestors may still
be queued and later records can add candidates. It is not a claim that all
project history, current code or memory has been verified. Exact file inversion
cannot establish an intra-file partial revert after intervening edits. Such
changes require stronger range evidence or explicit review; no whole-PR or
memory cancellation is inferred. Incomplete trees and commits exceeding the
16-parent read bound are surfaced as attention states, not silently indexed.

FS remains a development adapter with a process lock. It writes idempotent
index/queue dependencies before advancing the cursor so interruption replays
the page; it does not provide PostgreSQL multi-process transaction isolation.

## File applicability at an explicit code position

The application query `GET /repos/{repoID}/code-applicability` and read-only MCP
`code_applicability` compare a source change against an explicit full selected
Git SHA. They accept file paths and an explicit comparison parent for source
merge commits. They never infer the selected code from the latest publication
of a context snapshot. Web's on-demand code-state panel consumes that query;
branch folding, lane placement and historical PR completion are independent.

The observation worker now caches the selected commit's complete directory
objects. It rehashes the canonical Git tree representation, including modes,
symlinks, gitlinks, empty directories and directory-aware ordering. A clipped
response marked complete still fails the root hash check. Identical directories
share their Git OID, rather than copying the whole file list per commit. Blob
contents are not stored. Tree and later delta reads must agree on parents and
changed after-entries. Migration 0049 publishes nodes, the commit/root binding,
the observation fence and the evidence revision in one PostgreSQL transaction.
Previously completed scans without tree evidence are claimed for a resumable
upgrade. Existing proof, discovery cursor and context history are preserved.

Queries use one repository read generation, do no provider I/O and enqueue no
work. A bounded ancestry walk reports missing evidence or its visit limit as
unknown, never as a negative proof. A merge's current files come from its own
verified tree. Its first parent is not used as a substitute. The file states are:

- `applied`: source is in the selected ancestry and the file matches its after-entry.
- `before`: source is in ancestry but the file matches its before-entry. This
  alone does not identify a particular revert operation.
- `changed`: source is in ancestry and the file matches neither entry. This
  does not prove that all of the source's functionality is absent.
- `equivalent`: matching after-entry, but source is outside the selected ancestry.
  This is not a fabricated merge receipt; squash/cherry-pick provenance is a
  separate history binding.
- `not_in_history`: source is outside ancestry and its after-entry does not match.
- `unknown`: selected/source/index/ancestry evidence is incomplete, the merge
  comparison parent is ambiguous, or the source did not change the requested path.

A → unrelated B → inverse(A) therefore restores A's before-state while B stays
applied. Reapplication restores A's after-state. Sibling branches and another
worktree's selected SHA are independent. The response hash binds code selection,
repository generation and all returned evidence states. Original memory and
historical completion remain readable and unmodified.

Evidence progress has a separate durable revision counter. SSE updates invalidate
only Git discovery, verification and code-state queries; they do not download the
whole context graph. Actual history/refs still advance the graph counter. Older
clients can ignore the optional evidence counter; new clients retain the existing
graph invalidation fallback with older servers.

This is file-level evidence, not a natural-language memory classifier. Typed
claim provenance, effective memory selection, integrated-PR source bindings and
bounded active-session briefings remain required work under #208. A mixed legacy
memory fragment cannot be silently canceled merely because a source file changed.

### Verified document values and read-index compatibility

Document ingestion validates the CIR schema and canonical content hash before
publishing any snapshot. The application carries those exact bytes as a
request-scoped `VerifiedSessionDoc` through the optional `VerifiedDocStore` port.
The value has private immutable content; returned byte buffers are copies. It is
not a wire-supplied verification flag or a long-lived cache. Both FS and PostgreSQL
still validate repository identity and pre-existing blob integrity. PostgreSQL
publishes the body, ownership and read index in the existing transaction.

Read-index metadata now comes from canonical event bytes, so unsorted input cannot
associate another event's text, role or sequence with an offset. The v2 storage
namespace (`read-index-v2` / migration 0050 tables) isolates old projections during
rolling deployment. Missing v2 indexes are rebuilt lazily; the existing backfill
command can warm them. Archive bytes, snapshot IDs, memory hashes, natural parents
and graph facts are unchanged. Rebuilding may add latency on the first read;
retaining the old index namespace supports old replicas during rollout. Deleting
a document cleans its derived search data in both generations.

A synthetic approximately 4MB document reduced the CPU validation/index pipeline
from about 112ms and 125MB allocations to 61ms and 57MB on the development machine.
This does not measure provider, network or complete synchronization latency.
Cancellation during Git root discovery remains a cancellation/deadline error.
HTTP sync failures identify the method/path without exposing query credentials.

### Typed memory provenance and rolling compatibility

Memory fragments may contain explicit author-provided `claims`. Each is `code`,
`decision`, or `rationale` with text. Code claims additionally declare a full
source Git commit, an optional explicit comparison parent, and 1–20 exact
repository-relative paths. A fragment's `source_snapshot` supplies its context
provenance. This is a declaration, not proof of publication, current applicability,
or semantic truth. Those require the effective-memory query described below.
Automatic extractive/provider distillation remains untyped.

Authors can add a JSON array of claims with:

```sh
cxt memorize <snapshot-or-branch> --claims ./claims.json
cxt push
```

For example (replace the synthetic commit with the exact accepted source SHA):

```json
[
  {"kind":"code","text":"Login uses tokens.","code":{"commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","paths":["src/login.go"]}},
  {"kind":"rationale","text":"Retain authentication failure history for debugging."}
]
```

Import is additive. Subsequent automatic distillation replaces generated prose
while preserving explicit claims. Carry budgets apply to prose; explicit claims
remain attached and are not silently promoted to current code facts or inserted
into provider prompts. Limits are 256 claims per fragment, 2048 per digest and
8192 UTF-8 bytes per claim text. Exceeding them rejects the new write and retains
previous immutable generations.

Digests with claims use `claims_version: 1`; omitted optional fields keep existing
wire hashes unchanged. Large typed digests use the v4 storage chunk manifest.
The CLI uploads typed generations only through
`PUT /repos/{repoID}/typed-memory-attachments/{snapshotID}`. Older servers reject
that new path before mutation; clients never retry a legacy write route. Upgrade
the backend replicas before using typed writes. Mixed-version replicas can reject
new typed reads/writes until the rollout finishes, rather than discard fields.

The typed endpoint uses the same repository membership and causal pointer CAS as
ordinary memory. Untyped endpoints reject typed bodies. A causal child cannot
downgrade its parent's claims version. An explicit typed API replacement can clear
claims while retaining version 1; the original object remains readable. Concurrent
writers preserve their objects and only one pointer update from a given parent
succeeds. Never infer a publication mapping from timestamp order or graph placement.

Typed storage and authoring are the first delivery slice. Effective selection by
code SHA, verified PR integration anchors, Web/MCP consumption, and short deduplicated
active-session updates remain under #208. Stored/history reads continue to expose
the original memory; this slice does not yet classify claims as currently applied.

### Effective memory at an explicit code position

`GET /repos/{repoID}/effective-memory?snapshot_id=…&code_commit=…` is the
viewer-authorized application query shared by the web and remote MCP
`memory_load` with `mode=effective`. The immutable code SHA is required; neither
shared main nor a publication timestamp identifies a worker's code position.
An optional `memory_hash` pins an archived object belonging to that snapshot.
Otherwise the query composes the selected context's current memory lineage.

- Code claims require an accepted publication matching source snapshot and
  declared source SHA. Direct Git ancestry or a completed verified PR anchor
  must prove integration. A PR anchor requires matching source branch identity,
  immutable natural context ancestry, source Git ancestry within the PR head,
  and the merge SHA within the selected history. Mutable graft placement never
  establishes historical integration.
- All declared files matching the source after-state produce `applied` only
  with integration proof. All matching before-states produce `inactive` with
  sufficient history evidence. Partial inversions, later edits, missing evidence
  and equal bytes without integration stay `review`. This assesses declared file
  scope, not prose correctness or whether a named revert operation occurred.
- Decisions/rationale remain `retained` historical knowledge. Untyped prose is
  paged as unverified historical text, never guessed into code claims or erased.
- One repository read owns memory, history and Git evidence. No provider I/O,
  queue writes or position changes occur. Cached evidence reads are bounded to
  8192 per request; unresolved scopes remain reviewable.
- REST pages allow 1–50 items (default 20), at most 256KiB of item JSON. Cursors
  bind selection, lineage, history, graph and evidence generations; pending
  activity alone does not invalidate them. Changed generations require restart.
  MCP uses versioned item fragments within 12KiB responses across replicas.
- Web renders server states and uses React Query for pagination and revision
  refresh. Graph folding, current grafts and commit messages never decide
  applicability in the client.

CLI online load (full replay and managed memory) and branch seeds use this same
application query through a readonly port. `content=claims` excludes untyped
archive chunks without changing claim assessment or stable item IDs. This filter
is bound into cursor identity and echoed in responses; default archival paging
is unchanged. An older server ignoring the filter cannot be treated as a verified
prompt source.

- Read the actual target worktree HEAD, including detached HEAD. Never substitute
  shared main, another worktree, or a captured document's code SHA. The result is
  explicitly scoped to committed code, not dirty working-tree edits.
- Fetch at most four 50-item pages within a two-second operation deadline, with
  a 320KiB transport response bound. Main and departure roots share the allowance.
  Reject mixed generations, changed selections, repeated cursors, malformed
  pages and unsupported response contracts. An unavailable/incomplete server
  yields bounded, explicitly unverified history; caller cancellation still fails.
- A rewound selection uses its exact historical attachment, including an
  ancestor-owned memory object. An intentionally empty historical pin does not
  query the latest attachment.
- Reserve at most 16KiB inside existing seed budgets for state-labelled claims,
  source/context IDs and omitted-content retrieval guidance. All remaining
  summaries and replay are labelled history. `applied` proves declared file
  state with integration evidence, not semantic truth of the author's prose.
  No prose-only legacy statement becomes a verified current-code fact.
- Original digests and typed claims remain stored unchanged. Only the prompt
  copy includes assessments. New seeds replace prior generated assessments;
  inherited prompt text cannot carry an old selection's status into a new one.
- Recheck the target Git HEAD and worker-owned context/memory selection before
  installing the prompt. A checkout or same-code memory repin during preparation
  aborts installation. This check does not lock unrelated Git processes after
  it completes, and server generations can advance after a response; the prompt
  retains its explicit code/generation provenance.

Deduplicated active-session change notifications remain subsequent work. This
slice does not replace an already running provider transcript or inject a full
new seed on every observation.

### Memory retention during capture replacement

A successor's complete event prefix proves conversation coverage only. A capture
with an attached memory is retained even when its pending pointer moves and no
branch currently references it. Legacy server memory metadata also prevents
collection. CLI memorization holds the existing local object-retention lock from
selection through memory attachment.

Push negotiation is an observation, not a remote retention lease. If a later
memory attachment returns 404, a capable CLI restores the locally retained
snapshot, document and root memory through `POST /memory-publications`, then
replays the validated causal suffix before publishing refs. Staged chunks stay
immutable. PostgreSQL binds archive ownership and the root attachment in one
repository transaction; capture collection uses the same transaction boundary.
The development FS adapter serializes writers but does not claim database crash
rollback. Existing divergent attachments return conflict, unrelated errors do
not trigger restoration, and older servers are never downgraded to an unsafe
non-atomic repair. Normal already-present memory still uses the existing CAS
endpoint and hash-only manifest optimization.

### Bounded document upload

Sync negotiates missing document IDs before reading their bodies. It then reads,
validates and uploads one cumulative document at a time, and publishes snapshot
metadata only after every required document is available. Memory, graft/history
and ref publication keep their existing ordering. Pending and unsync uploads use
the same object path. A failed or canceled upload does not advance those pointers;
completed immutable documents/chunks can be reused by the next negotiation.

This bounds decoded-body retention to the largest individual document rather
than the sum of the backlog. It does not claim constant memory independent of
document size: one document still requires reconstruction, JSON decoding and
canonicalization. Cancellation is checked between documents and stored chunks,
and after decoding; a single JSON decoding/hash operation is not preemptible.

### Durable document finalization

Peers advertising `async_docs_supported` accept one repository-owned staged
manifest at `POST /push/doc-jobs`. Acceptance returns 202 and a deterministic
job ID; it does not publish a snapshot or move a ref. `GET /push/doc-jobs/{id}`
reports waiting, running, retrying, completed or rejected. The CLI waits through
short HTTP requests and replays acceptance after interruption. Existing HTTP and
hook deadlines are unchanged; cancellation stops waiting but leaves accepted
server work durable. Older servers retain synchronous `/push/objects` behavior.

The worker verifies each chunk, reconstructed hash and typed CIR canonical form
outside the repository transaction. Renewable leases and versions fence stale
workers. PostgreSQL commits the verified body/read index and completion receipt
in one transaction. Exact manifest bytes use a `bytea` payload: JSONB would alter
key ordering or number spelling inside the canonical envelope. Queue capacity is
32 pending documents per repository; each manifest is bounded to 256 KiB and
2048 chunks, and each assembled body to 512 MiB. One worker runs per process;
claims serialize a repository across replicas. Temporary failures retry with
bounded backoff; invalid documents remain rejected and never publish refs.

FS is a single-process development adapter: an interrupted cross-file write is
replayed idempotently, not advertised as database ACID. Repack retains chunks
referenced by pending jobs. A completed body removed by ordinary collection is
verified again if later submitted.

This removes document validation from the HTTP request lifetime. It does not
claim constant per-document memory, short storage-index transactions, or eliminate
subsequent snapshot/reference integrity checks. Large-body operational validation
must cover those remaining phases before declaring backlog recovery complete.


### Join policy ownership

Manual same-branch session repositioning uses `domain.PlanJoin` over a complete
repository graph. The application loads snapshots, refs, pending pointers and
durable branch memberships inside the repository write boundary, allocates any
residual session ref, and persists the domain mutation. The planner never changes
natural parents or immutable PR completion records. It rejects unpublished source
segments, natural ancestors, ambiguous first-parent descendants and mutations
that affect another branch. A stale pending pointer does not turn a shared commit
back into an uncommitted capture.

Both storage adapters call the same domain mutation and graph-scope validators.
PostgreSQL still locks the repository and verifies head/graft generations inside
the transaction; FS still journals prepared/committed changes for crash recovery.
Neither adapter may trust an earlier plan after another child or foreign ref has
been attached. Multi-root reachability traverses a shared ancestor once per scope,
not once for each branch/session/lifecycle ref.

The browser uses `GET /join/preview` for branch choices, typed rejection reasons,
segment count and allowed drop targets. It no longer derives eligibility from
folded nodes, branch labels, natural ancestry or graft reachability. Selection,
drag state, folding, tooltips and coordinates stay in React; React Query owns
preview loading/cancellation. Right-clicking a snapshot opens the same preview
and permits an explicit branch choice when membership is ambiguous.

The user-facing `ConfirmJoin` command requires the preview's expected head and
content-derived plan revision. The whole-segment and source-only choices have
separate revisions. Revisions are not signed credentials and grant no rights.
Confirmation repeats membership/archival checks inside the write transaction;
PostgreSQL locks workspace policy and actor membership until commit. The former
unapproved public Join application method is removed.

A revision includes the exact segment, graft patches, branch identity and a
canonical published-graph scope hash. Both FS and PostgreSQL recompute the scope
hash under the ApplyJoin lock, including ref attachments and graft generations.
This rejects a stale approval even when the selected head has not moved. Pending
transcript growth and display metadata alone do not invalidate the preview. A
changed plan returns `409 join_preview_changed`; the browser preserves selection
and requires a fresh preview and a new explicit confirmation. Query failures
never fall back to browser business logic. FS retains its development-only
prepared/committed journal guarantee; PostgreSQL provides the transactional
policy/graph boundary used by production.

Verification includes real HTTP preview/confirmation contracts, two PostgreSQL
services confirming one approval (one winner), stale topology and permission
checks, and a full-stack browser test using actual cxtd handlers. The browser
case covers query failure, concurrent ref changes, renewed approval and preserved
natural ancestry; transport errors are injected without replacing the server's
business response with a shared mock.

### Incoming PR discovery before fetch

The post-merge/post-rewrite path durably records incoming Git commit ranges before
fetching context from the server. Otherwise a failed first fetch would return
before recording ORIG_HEAD..HEAD, and a later Git operation could replace that
only discovery source. Storage failure is reported without claiming the work was
queued; no context promotion is attempted from an unrecorded range.

Saved discovery for the current Git origin/base branch also retries after an
ordinary successful `cxt push` or `cxt pull`. It no longer requires another Git
merge event. Existing limits (200 commits per batch, at most four batches per
replay), immutable exact PR source checks and idempotent server promotion remain.
Only a completely processed range is acknowledged; provider or promotion failure
keeps it available. Replaying discovery does not restore provider transcripts or
invent a branch/merge relationship.

Completed PR replay may return the context target recorded for an older merge.
If validated local reachability already includes that target in the current
branch, reconciliation preserves the newer local ref and acknowledges the old
result without a remote ancestor walk. The 256-snapshot remote search limit
still applies to genuinely unproven paths. Replaying a historical completion
must neither rewind the worktree nor exhaust the hook before later PR discovery.


Exact PR promotion completion and local reconciliation are separate results.
The CLI validates the returned repository, base branch, branch identity and
snapshot hash before durably acknowledging the exact PR delivery. A subsequent
fetch or bounded local ancestry reconciliation failure reports
`ErrPRLocalReconciliation`: server completion is confirmed, local progress is
preserved, and discovery can acknowledge that Git range and process later PRs.
Transport errors before server confirmation, malformed responses and failed
local acknowledgements remain retryable discovery work. Ordinary pull/push
continues local reconciliation; a completed server PR never authorizes a forced
local ref move or makes a divergent local branch a failed remote promotion.
