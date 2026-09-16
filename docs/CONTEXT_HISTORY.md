# Context branches, working positions, and retained history

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
  committed operations or a verified appended reflog creation. Unprovable
  prepared operations remain queued. Empty replay cannot bind a repository
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
must match a real branch ref movement. Without a receipt, a join requires both
an existing append edge and an unambiguous source branch. A pending receipt,
shared current hash, or ordinary fast-forward alone does not prove a merge.

At a join, main continues along its previous main history; the source branch
remains on a side lane between its recorded birth and join. These event nodes
are a read projection: clicks open the archived conversation at that event;
no synthetic snapshot or rewritten parent is stored. Rename and name reuse
retain distinct birth identities. Orphan branches begin without conversation
parents. An unborn orphan without a capture remains in the operations list.

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
