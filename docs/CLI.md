# `cxt` CLI reference

`cxt` is the Git-integrated client for capturing, restoring, and synchronizing
coding-agent context. This reference describes the public command surface on
the `main` branch.

Check the installed version and built-in command catalog first:

```bash
cxt --version
cxt --help
```

For installation and first-run setup, see
[Installing CXTHub](INSTALLATION.md).

## Conventions

- Run repository commands inside an existing Git working tree.
- New connections use `https://<host>/<owner>/<repository>`. The server resolves
  this canonical display address to the original connection identity before it
  is saved. Existing two- and three-segment remotes remain valid exact aliases;
  their normalized URL and RepoID are never rewritten during ownership migration.
  An unavailable server prevents saving a new unverified connection, while
  existing connections and local captures continue to work offline.
- `<ref>` accepts `HEAD`, a branch name, a tag name, or a full
  `sha256:<64-hex-character>` snapshot ID. An omitted ref resolves to the
  current context head where supported for archive reads and context operations.
  Managed fresh input uses the latest authorized server `main` independently of
  that local selection.
- Providers are `claude` and `codex`.
- Explicit legacy archive restore modes are separate from prepared fresh input:
  - `full`: materialize the native session when possible;
  - `reconstructed`: rebuild a provider-compatible session from normalized
    context; and
  - `memory`: inject the distilled memory representation.
- Managed Git branch creation requires a durable local creation record before
  Git commits the ref transaction. Failure to record that evidence blocks
  creation. Provider capture and network synchronization failures are reported
  and retried without blocking Git or terminating an agent.
- Every public command supports `-h` and `--help`. Help and usage errors are
  resolved before cxt creates adapters, contacts a remote, or changes local
  context state. Unknown flags, missing values, invalid provider/mode values,
  duplicate options, extra positionals and unsupported option combinations are
  rejected. `-f` and `--force` count as the same option. `push`, `pull` and `fetch` accept a configured remote name and optional
  branch. Repository identity is verified; unsupported refspec mappings fail. Value
  flags also accept `--flag=value` (or `-m=value`) when the value itself starts
  with a hyphen.
- `cxt claude` and `cxt codex` pass provider-owned arguments through. Use a
  `--` separator when the provider itself should receive a help flag, for
  example `cxt codex -- --help`; without the separator, `--help` describes the
  cxt wrapper command. The Claude wrapper forwards only auto-memory-relevant
  launch metadata from isolated inline `--settings`, `--setting-sources`,
  `--bare`, and `--safe-mode` to cxt hooks; unrelated settings values are never
  copied into the environment. External or mixed-purpose `--settings` remain
  provider-owned and make native-memory projection fail closed.
- Inspection commands (`log`, `list`, `branch list`, `remote`, config reads,
  `tag` without a name, `stash list`, and `settings list`) do not replay pending
  branch operations. Use `cxt branch replay` or synchronization to retry them.
  `fsck` and `reflog` identify the configured replica without registering it on
  the server. Missing remote state is reported by the read endpoint; inspection
  does not create it. `settings list` computes current hashes without saving
  configuration objects.

## Recommended onboarding

### `cxt setup`

```text
cxt setup [remote-url] [--no-login]
```

Runs the complete, idempotent onboarding sequence:

1. initialize the local `.cxt` store;
2. install managed Git hooks;
3. register the repository remote when supplied;
4. authenticate through the browser device flow unless `--no-login` is set;
5. merge Claude Code and Codex lifecycle hooks; and
6. pull team settings when authenticated.

Example:

```bash
cxt setup https://cxthub.example/alice/platform
```

Rerunning the command reports and repairs missing setup steps without replacing
an existing remote that points somewhere else.

The lifecycle hooks also cover Codex app/IDE sessions and Claude Desktop's
**Code** tab. Apps are not launched through a cxt wrapper: hook payloads identify
the exact session and linked worktree, and CXTHub stores all worktrees in the
primary repository's shared `.cxt`. Concurrent app sessions retain independent
capture cursors and pending pointers.

Codex's provider hook is global, but cxt activation is not. A Git repository is
capture-enabled only after `cxt init` or `cxt setup` creates `.cxt/HEAD`.
Directory-only residue from older hooks is kept intact and automatically added
to both `.gitignore` and `.git/info/exclude`; it is not treated as an initialized
context store.

## Repository and authentication

### `cxt sync status`

```text
cxt sync status [--json]
```

Reads local retained-history upload jobs and unresolved branch operations without
opening context or memory bodies, contacting the server, or retrying work. Use it
for a quick queue inspection while a large synchronization is in progress.
Eligible jobs can be retried now; that count does not mean a worker is running.
An empty queue does not establish server acknowledgement or object integrity.
Unreadable queue metadata produces a nonzero exit status without repairing it.

Run `cxt push` to retry current work and wake retained-history uploads, or
`cxt push --wait-history` to wait for the retained backlog. Explicit `push` and
`pull` print phase progress to stderr. Item counts advance after acknowledgement
and apply only to the named phase, not to the entire synchronization. Use
`cxt doctor` for a complete local object audit and `cxt fsck` for a server audit.

### `cxt doctor`

```text
cxt doctor [--json]
cxt branch operations [--json]
cxt branch replay
```

`doctor` inspects the local replica's snapshot objects, refs, history roots,
hashes, and pending save/branch transactions. Inspection is read-only and works
without initializing a missing `.cxt`. Issues produce a nonzero exit status.
It does not contact the server or assert that local data has been uploaded.

V2 chunked documents are verified from current bytes one chunk and one event
at a time, including the complete reconstructed hash and the ordinary reader's
JSON validation. Legacy document formats retain their existing full verification.
This reduces retained document memory; it does not skip content checks or
guarantee a constant memory bound for a single oversized event.

Within one inspection, successful current event JSON validation may be reused
by its exact byte hash when most chunks have already been observed. Bounded
hash markers retain no dialogue payload and are discarded when inspection ends.
Every document still reads its current files and verifies its complete hash,
envelope, JSON syntax and depth. Unique chunks use the direct decoder; repeated
chunks with too many event misses fall back to it. These cost heuristics do not
certify validity or guarantee that every workload becomes faster.

`Ctrl-C` cancels the inspection at cooperative read/event boundaries. Doctor
prints the partial report and exits 130. JSON includes `completed`,
`replica_inspection_completed`, `documents_checked`, and `unchecked` phases.
The snapshot count is metadata discovered, not the number of documents verified.
`completed: true` means all phases finished, even when `issues` contains damage;
an interrupted inspection cannot certify the unchecked portion. This is a
read-only observation of current files, not an atomic disk snapshot.

`branch operations` reads the Git-directory journal even if `.cxt` is damaged
or missing. `branch replay` retries committed operations and prepared operations
with an exact Git reflog witness. Unproven operations remain pending and are
reported as `needs-git-evidence`; no branch birth is inferred from a name alone.
Replay queues server synchronization. Run `cxt push` to wait for acknowledgement.
For a prepared orphan operation whose committed callback was lost, inspect the
operation and run `cxt branch recover <operation-id> --confirm-orphan` in its
recorded worktree. This works only while HEAD still names that exact unborn
branch. It records user confirmation as separate evidence, verifies inherited
memory, and replays the operation. A moved/committed branch is not guessed.

### `cxt repair --from-server`

```bash
cxt repair --from-server
# Missing/damaged remote configuration:
cxt repair --from-server --remote https://cxthub.com/owner/repository
```

`--remote=https://cxthub.com/owner/repository` is equivalent to the spaced
form. Repeated `--remote` options are rejected before repair starts.

Repair requires the repository identity recorded under the Git common directory.
It downloads and verifies an isolated server replica first, then restores missing
or corrupt objects in place. Every replaced predecessor is quarantined under
`.git/cxt/repairs/<id>/` before atomic replacement. Healthy local-only objects,
ahead refs, and worktree positions remain intact. It does not rewrite provider
sessions or send any repair writes to the server. Config can be restored only
when the supplied URL matches the durable identity.

Interrupted repairs are safe to retry. A corrupt server copy, identity conflict,
or failed quarantine stops replacement. Local-only damage without another valid
copy and invalid local transactions remain explicit errors; repair cannot infer
who caused the damage or reconstruct missing bytes. Run `cxt doctor` afterward,
then `cxt branch replay` to process verified Git operations.

### `cxt init`

```text
cxt init [--no-hooks] [--remote <repository-url>]
```

Initializes `.cxt/`, updates local ignore protections, creates `.cxtsecrets`
from eligible `.env` values when appropriate, and installs managed Git hooks.

- `--no-hooks` skips Git hook installation.
- `--remote` also registers `origin`.

Local-only initialization:

```bash
cxt init
```

### `cxt repo create`

```text
cxt repo create <repository-url>
```

Convenience alias for initialization plus `origin` registration.

### `cxt remote`

```text
cxt remote [-v]
cxt remote add <name> <repository-url>
cxt remote remove <name>
```

The server resolves the display URL to its stable connection identity before
registration. The saved `origin` determines the API endpoint and content ID;
renaming a repository does not create a new context history.

```bash
cxt login --server https://cxthub.example
cxt remote add origin https://cxthub.example/alice/platform
cxt remote -v
cxt remote remove origin
```

Changing an existing remote requires removing it and adding the replacement.

### `cxt login`

```text
cxt login [token] [--server <server-url>]
cxt login -t <token> [--server <server-url>]
```

Without a token, starts the browser device flow. Use `--server https://<host>`
to authenticate before a new connection; otherwise it uses the configured
`origin`. `cxt setup <repository-url>` performs login and connection together.
The manual token form is intended as a fallback and may leave the token in
shell history. `CXT_TOKEN` is the non-interactive override.

### `cxt logout`

```text
cxt logout
```

Removes the locally stored credential for the configured origin host. It does
not revoke the token on the server.

## Agent and integration commands

### `cxt claude`

```text
cxt claude [claude-arguments...]
```

Runs Claude Code with fresh input from latest authorized server `main` and
passes remaining arguments to the installed `claude` executable. The wrapper
owns process restart/resume on a branch switch. It is optional for Claude
Desktop's Code tab, where lifecycle hooks preserve the live app session and
apply one bounded memory handoff from latest authorized server `main`.
Existing native resume does not reinject context.

### `cxt codex`

```text
cxt codex [codex-arguments...]
```

Runs Codex with fresh input from latest authorized server `main` and passes
remaining arguments to the installed `codex` executable. The wrapper owns
process restart/resume on a branch switch. It is optional for Codex app/IDE
sessions, where lifecycle hooks preserve the live app session and apply one
bounded memory handoff from latest authorized server `main`.
Existing native resume does not reinject context.

All managed fresh injections use this source rule: default memory, explicit
budgeted history, `load` delivery, branch seeds and desktop handoffs. The server
resolves `main` directly; the local branch/worktree, local `main`, local Git
commit and rewound memory cursor cannot select its tip. Source selection
`branch`, `snapshot_id` and `code_commit` identify main. The additive, optional
`source_policy: latest_server_main` and `working_position` fields record that
policy and the separate actual Git branch/code being edited.

Missing or denied server main, invalid source data and memory errors stop the
injection without local fallback. Source and memory are reauthorized and
revalidated before delivery, along with the actual working position. Concurrent
changes abort delivery or require fresh preparation; these checks are bounded
observations, not a distributed transaction across the server, Git and provider.
Verified-empty new-repository bootstrap remains a distinct first-launch path
requiring an authorized empty server repository; it never invents `main` or
treats missing main in a nonempty repository as empty. See
[context input and bootstrap](CONTEXT_INPUT.md).

### `cxt mcp --local`

```text
cxt mcp --local
```

Starts the optional read-only **offline-development** MCP helper on standard
input/output. It reads the current repository's `.cxt` working replica and is
distinct from the default product connector at `https://cxthub.com/mcp`, which
runs in `cxtd` against cloud PostgreSQL. Bare `cxt mcp` is rejected so a local
replica cannot be mistaken for the shared product data source.
See [MCP connections](MCP.md). The local helper exposes:

| Tool | Purpose |
|---|---|
| `context_list` | List local repository snapshots |
| `context_fetch` | Fetch snapshot metadata, memory, and recent conversation context |
| `memory_load` | Load the bounded memory projection across all natural and graft parent lineages for a ref |
| `context_search` | Search synchronized team context through the configured origin |

### `cxt hook`

```text
cxt hook --provider <claude|codex> --event <event>
```

Provider-integration entry point. `cxt setup` writes these invocations into
provider hook settings; users normally do not call it directly. Hook failures
are reported without blocking the provider.

Supported coding-app events are `SessionStart`, `UserPromptSubmit`, `Stop`, and
`SessionEnd`. A desktop branch switch never renames the vendor-owned active
session file. Instead, the next start/prompt hook consumes a session-scoped,
maximum-16-KiB project-memory handoff from latest authorized server `main`
after source/memory revalidation. Successful delivery acknowledges the queued
request; a process crash between output and acknowledgement can cause a retry.
Its input is independent of the
selected local branch or historical cursor. Full transcripts remain in
the immutable CXTHub DAG. The web viewer exposes archived conversation; current
MCP tools provide bounded history results with the limits documented in
[MCP connections](MCP.md#current-history-retrieval-limits). A per-handoff limit
does not bound the total input accumulated across many branch switches.

Claude Desktop's general Chat tab is outside this hook model and is not
passively captured.

Start/prompt hooks also launch one background observer for their validated
session in that exact worktree. While the transcript changes, it captures and
syncs the uncommitted pointer about every 10 seconds; it never advances a
branch ref or inserts team conversation into the active provider session.
Every sample resolves the current Git position afresh. Failed uploads retain
the local pointer and retry even when no more text is written. Duplicate
observers are excluded by an OS lock. Observation ends when the session
registration is removed, after 30 minutes without transcript activity, or after
24 hours; a later start/prompt hook starts it again.

Capture and publication run as separate bounded steps. A saved capture is the
retry checkpoint: upload failures retry that capture before reading newer
transcript growth. Acknowledged server documents are not reopened on a pointer
retry. Large sessions can take longer than one observation interval to process.

On Hold checks for updates every 5 seconds and follows the selected session's
latest snapshot. LIVE means transcript activity within the last two minutes, not a
guarantee that the app or network connection is still open. The badge expires
without further activity, including while the server is unreachable. Replaying
an old pointer does not refresh its activity date. Hidden sessions stay hidden,
and committed captures remain on the shared timeline.


## Capture and commit commands

### `cxt add`

```text
cxt add [claude|codex|.]... [--expect <index-revision>] [--json]
```

Freezes matching native sessions in this Git worktree into a versioned index.
With no provider, or with `.`, both providers are considered. Adds are cumulative:
adding Codex does not remove staged Claude sources. Each entry records its exact
source identity, generation, immutable document and event range. Later native
conversation edits cannot silently change what a manual commit publishes.
`--expect` performs a compare-and-swap against the displayed index revision.

### `cxt commit`

```text
cxt commit [-m <message>] [--expect <index-revision>] [--json]
cxt commit --resume <operation-id> [--json]
```

Publishes only the frozen index. An empty index is an error; run `cxt add` first.
Commit does not reread live provider files. Its durable operation journal lets a
retry finish the same publication without consuming concurrently added entries.
A local finalization receipt is not evidence of server acceptance; publish with
`cxt push` and inspect upload status separately.

Ordinary `git commit` captures eligible sessions from both providers independently.
The automatic hook does not consume or clear the manual frozen index. The old
provider-selector `staged` configuration is ignored: it contains no frozen content
and cannot safely be migrated into an index.

### `cxt restore`

```text
cxt restore --staged <source-key>...|. [--expect <index-revision>] [--json]
```

Removes selected entries (or all entries with `.`) from this worktree's index.
Raw archives and native sessions are preserved. `cxt stash push --staged` saves
this index; `cxt stash list --staged` lists it without mutation; restore it with
`cxt stash pop --staged --id <stash-id>`. Incompatible entries fail without
silently replacing newly staged sources.

### `cxt status`

```text
cxt status [--json]
```

Reads the actual Git SHA, selected context and pinned memory, index revision,
staged event ranges, stored pending captures and local commit receipts. It does
not scan provider files, contact the server or replay journals. Pending captures
are repository-scoped; their presence does not prove a session is currently live.
Unavailable watcher/server evidence is reported as unknown, not clean.

### `cxt diff`

```text
cxt diff [--staged] [--json]
```

The default compares stored pending captures with the index or selected HEAD.
`--staged` compares the frozen index with finalized coverage reachable from the
selected history. Events are compared by verified source identity and canonical
content, never by timestamps or source-file length alone. Replaced, older and
unavailable sources remain distinguishable.

### `cxt fetch`

```text
cxt fetch [remote [branch]]
cxt pull [remote [branch]]
cxt push [remote [branch]] [--force|--append] [--wait-history]
```

A named remote is verified against the local immutable repository ID before any
sync mutation. Unknown names, other repositories and unsupported refspecs fail.
An explicit branch scopes pointer updates; immutable dependency objects and
repository evidence can still be transferred. Credentials from the configured
origin are never forwarded to a different server; log in to that server separately.
`push origin [branch]` uses the same current-work-first/background-history policy
as `push`; only `--wait-history` waits for the origin backlog. Other named remotes
complete retained uploads in the foreground because the background queue is
currently bound to origin. They never wake a worker for a different destination.

`fetch` updates a separate remote observation and immutable cache. It leaves the
working context, applied memory, index and branch refs unchanged. `pull` also
reconciles permitted pointers and pins a complete authorized context/memory
projection to the selected Git code. It does not execute an agent or edit an
ongoing conversation. Local-ahead refs are preserved.

`pull --force` and `pull -f` fail argument preflight before composition, queued
branch/PR replay, or sync work (exit 2; mutation state `unchanged`). To resolve a
conflict, preview one pointer and apply exactly that plan:

```text
cxt repair --preview --ref feature --reason 'adopt reviewed remote tip' --output repair.json
cxt repair --apply repair.json --expect sha256:<plan-id>
```

For a memory conflict replace `--ref feature` with `--snapshot sha256:<snapshot>`.
The first apply rechecks remote authorization/revision, Git position and local
pointer/index state; intervening changes invalidate the plan. Losing refs/digests
are retained. Apply output identifies the durable receipt, its `state=applied`
and original `applied_at` time. Repeating the same approved plan returns that
existing receipt without reapplying or rechecking current authorization,
remote revision, or local position. The receipt records the completed operation;
it does not assert that its pointer is still current.
Plan files contain private context metadata and should not be committed.

### `cxt save`

```text
cxt save [-m <message>] [--provider <claude|codex>]
```

Creates a manual snapshot for one provider. An explicit `--provider` wins. If
it is omitted, cxt follows the provider owned by the live `cxt claude` or
`cxt codex` wrapper; in a plain shell it chooses the most recently updated
capture-eligible session in this worktree. Stale inherited wrapper markers are
ignored. A managed wrapper also binds the command to its exact native session,
so a newer same-provider session in another terminal cannot be captured by
accident.

The command does not consume `cxt add` staging and does not schedule the commit
path's detached remote pending synchronization.

Use `cxt commit` when the capture represents a code commit. Use `cxt save` for
an explicit standalone checkpoint.

### `cxt list` and `cxt log`

```text
cxt log [<ref>|--branch <branch>|--all|--retained] [--server] [--json]
cxt list [<ref>|--branch <branch>|--all|--retained] [--server] [--json]
```

`log` and `list` share one read contract. The default walks this worktree's
selected context HEAD, including natural parents and graft parents. A capture's
original branch label does not decide membership. Children appear before their
parents even when capture timestamps are out of order; independent captures use
newest timestamp, then content ID as a stable tie-break.

- `<ref>` or `--branch` selects a context ref. Ambiguous branch/tag names fail.
- `--all` walks all known local ref targets. `--retained` inspects every locally
  retained snapshot, including records outside the selected history.
- `--server` queries the cloud's shared context projection for the selected
  source and actual Git commit. It preserves the server's inclusion order,
  evidence revision, and state hash. It does not combine with `--all` or
  `--retained`, and an unavailable/denied server never silently becomes a local
  answer.
- `--json` returns versioned selection, source, state hash, coverage gaps and
  server evidence. A local result says `server_checked: false`; an incomplete
  ancestor chain is explicit. A local state hash includes mutable memory and
  graft metadata, not just snapshot IDs.

These reads do not register repositories, replay pending operations, capture
providers, or move refs/working positions. Changing from the old label-filtered
list to ancestry can change which rows appear; it does not delete stored data.

## Restore and branch commands

The accepted behavior for exact code/context positions, orphan memory
inheritance, retained progress, and retryable branch creation is tracked in
[Context history](CONTEXT_HISTORY.md). Worktrees share immutable objects but
retain independent code/context positions; moving one worktree does not rewind
another worker or delete server history.

### `cxt checkout`

```text
cxt checkout [<ref>] [-b <new-branch>]
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

Restores a ref. With `-b`, creates and restores a new context branch from that
ref. Git checkout hooks normally invoke the corresponding behavior
automatically. A branch's natural parents and archival memory follow its actual
creation source, including an explicit historical ref. Managed fresh seed input
and desktop handoffs use latest authorized server `main`; that input does not
rewrite branch ancestry, archival memory or the worktree's selected cursor.
An orphan's inherited archival memory remains distinct from prepared main input.

Explicit legacy replay restores the selected source archive instead of preparing
a fresh input package. In that restoration path, conversations that fit are
preserved in full; larger sessions keep a bounded recent tail starting at a
user-turn boundary and distill the exact omitted slice into a bounded bridge.
That bridge is merged with the inherited compact/project memory, so work after
an older provider compaction does not disappear between the summary and recent
tail. The source snapshot remains immutable and reachable; the inherited
memory plus bridge is attached to the seed for future memorize and branch
operations without enlarging the provider prompt budget. When current
conversation events are also replayed verbatim, their extractive memory delta
is removed only from the prompt projection, so the new session receives each
turn once while the full digest remains attached to the seed.
Provider-native baselines follow the same prompt-only rule with an explicit
scope. Claude auto memory is resolved from the canonical Git repository, so
linked worktrees and subdirectories share the same source.
`CLAUDE_CONFIG_DIR`, safe `CLAUDE_CODE_PROJECT_DIR_NAME`, isolated
user/inline-flag/managed `autoMemoryDirectory`, settings precedence, and
auto-memory disablement are honored. The supervised wrapper fingerprints every
observable settings input at launch; if the profile is absent, changes later,
uses an external or mixed-purpose settings document, invokes a policy helper,
or uses an unmodeled remote memory store, cxt does not ingest that native file.
Even after the file is resolved, cxt does not selectively strip the Claude
`MEMORY.md` baseline from the normal budgeted projection: configuration cannot
attest that the target runtime loaded those exact bytes, because model/runtime
gates and a startup-time file change remain possible. Exact-baseline merge
deduplication keeps one copy rather than recursively nesting it across seed
generations. Codex
`memories_1.sqlite` memory is retained because it belongs to the source thread
and the seed receives a new thread ID.

### `cxt switch`

```text
cxt switch [<branch>] [-c <new-branch>]
  [--mode <full|reconstructed|memory>]
```

Git-style alias for checkout. `-c` creates a branch.

### `cxt fork`

```text
cxt fork <ref> --as <branch>
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

Creates a context branch from a specific ref and restores it. The ref determines
natural ancestry and archival memory. Any managed fresh input uses latest
authorized server `main`; explicit legacy replay remains archive restoration.

### `cxt branch` / `cxt branch list`

```text
cxt branch
cxt branch list
```

Lists active local context branch refs and their targets, with `*` marking the
selected named context HEAD. This is a local observation, not a server freshness
check. It does not create branches, replay queued operations, load conversations,
or restore archived records.

### `cxt branch archive` / `cxt branch restore`

```text
cxt branch archive <name>
cxt branch restore <name>
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

`archive` removes only the active context-branch pointer after the matching
Git branch has been deleted. It first records an immutable lifecycle tag, so
all snapshots, conversations, compact memory, and graph ancestry remain
reachable and syncable. Git's branch-deletion hook performs this automatically;
the command is also available for repairing historical stale pointers.

`restore` resolves the latest archived target, records a newer active lifecycle
event, recreates the context pointer, and restores the provider session. Managed
fresh delivery uses latest authorized server `main` while preserving that
archived lineage; explicit legacy replay restores the archive. A stale client
cannot recreate an archived pointer by an ordinary push; it must
observe or explicitly create the newer active generation.

### `cxt load`

```text
cxt load [<ref>]
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

Prepares managed fresh input from latest authorized server `main` without
creating a branch or moving HEAD. For delivery, an explicit ref or rewound
memory cursor does not replace main as the prepared source. With no explicit
replay mode, the package supplies main's structured project memory, main source
metadata, separate actual working position, coverage gaps and MCP source
references. Raw transcript is not automatically inserted. Exact personal work
state is included only when an explicit principal/session/worktree scope is
available; missing scope is reported, never replaced with a teammate's tasks.

When `--provider` is omitted, managed `load` can infer the provider from the
authorized server main snapshot metadata. The package's optional `provider`
field carries that choice; no local archive is required for this inference.

Only an explicit `--mode` on the current command selects legacy archive replay.
Without it, commands use structured managed input from latest authorized server
`main`. Stored `load.mode` and account server preferences remain for
compatibility but cannot override managed injection. Git hooks always use
structured managed input, regardless of those stored preferences.

Explicit legacy replay modes retain archive restoration for the selected ref;
they are not prepared fresh injection. `full` and `reconstructed` restore the
archived conversation. Oversized replay distills the omitted span before
materializing a session, uses user-turn boundaries, and preserves the original
archive. Replay fidelity is separate from the new history input token budget.

Without an explicit ref, `cxt load --output` previews the latest authorized
server `main` input, including the `200k` and `full` examples below. An explicit
`<ref>` instead selects historical archive inspection and its exact memory
revision. Both paths produce inspection artifacts only; neither launches a
provider nor changes its files:

```text
cxt load --provider codex --model gpt-5.4 --context-budget 200k --output context.json
cxt load --provider claude --context-budget full --output context.json
cxt load <historical-ref> --provider codex --model gpt-5.4 --context-budget 200k --output historical-context.json
```

Existing files are not overwritten. Artifacts contain private memory and dialogue;
keep them out of Git. `full` means a ceiling of 800,000 tokens, not a promise that
a model can accept that much. A documented `--model` mapping uses an offline
text tokenizer; omitted/unknown models and Claude use explicitly inexact UTF-8
byte allowances. `usage.scope: text` excludes native-host inputs outside the
rendered package. Output artifacts cannot be relabeled for delivery; a
managed fresh injection must prepare and revalidate latest server main and its
memory. Provider acceptance remains unverified. See
[local accounting and resource limits](CONTEXT_INPUT.md#local-text-accounting).

The interactive wrapper also recognizes:

```text
cxt --pull --context-budget 200k codex --yolo
cxt --pull --context-budget full codex --yolo
cxt --pull codex --yolo
```

These managed history requests use latest authorized server `main`, independent
of the local branch or historical cursor. The final two commands request the
same 800,000-token package ceiling. Requests in 100k steps are supported.
A verified runtime's total initial input is limited to 80% of its window;
host input/framing is deducted before selecting recent
complete turns. Required output reserves or an earlier compaction trigger can
lower that limit further. Requested, effective and selected budgets are recorded
separately, without shortening stored source records. **Strict native history
launch is currently unavailable in the shipped runtime adapter:** no installed
host/model/tokenizer combination has been verified. These commands report
`provider_capability_unknown` before materializing or launching; an unknown
window is not replaced by a 1M guess or a hidden memory-only launch. Verified
small-window adjustment is supported by the preparation policy, while the
shipped runtime still lacks the required capability evidence. The artifact path is
available for inspection. No global model-window or auto-compaction setting is
changed. Native resume, provider help and noninteractive commands preserve their
provider-owned behavior and receive no injected package.
Native 200k/full-budget delivery, real-host acceptance and interactive TUI
handoff remain incomplete validation gates.

The explicit legacy `--mode memory` archive-restoration path keeps the full
immutable digest in cxt storage but projects at most 64 KiB into the target
provider's instruction file. cxt appends or
refreshes one marked region in `CLAUDE.md` or `AGENTS.md`; text and permissions
outside that region are preserved. Malformed or duplicate cxt markers fail
closed instead of guessing a destructive replacement range.
The provider-visible projection may remove a working-tree-scoped native prefix
only when the provider supplies an exact runtime load attestation. Claude does
not currently expose one, so its full resolved baseline, conversation delta,
and every unrelated lineage fragment remain portable. Session-scoped native
memory is likewise carried into the managed region so a newly materialized
provider session cannot lose it.

### `cxt memorize` and `cxt memory`

```text
cxt memorize [<ref>] [--provider <claude|codex>]
cxt memory [<ref>] [--provider <claude|codex>]
```

Distills a snapshot into reusable memory and attaches it for the next push.
With no ref, uses the current branch head. `memory` is an alias for
`memorize`; there is no `cxt memory save` subcommand. Modern snapshots select
their recorded source provider automatically. An explicit `--provider` must
match it; cxt never imports unrelated native memory from another provider or
terminal into that snapshot. A provider compact summary or native memory is
kept as the long-term baseline, and meaningful user/assistant turns after that
baseline are attached as a deterministic bounded conversation delta. Repeated
baseline text is rendered once across merged lineage fragments.

Immutable memory objects retain their original structured fields for audit and
recovery. Before cxt places those fields in a provider prompt, branch seed,
managed memory file, MCP response, or a carried generation, it creates a
non-mutating prompt projection. Legacy tool/provenance entries are omitted, and
an `open_tasks` list is treated as active work only when it came from a
provider-structured extraction. Extractive or legacy task lists remain in the
archive but are not reintroduced as instructions. A versioned hidden marker in
new cxt seed text preserves authoritative task lists and explicit empty task
tombstones during memoryless recovery; unmarked historical seed sections are
never promoted to authority retroactively.

Provider compaction summaries can themselves be cumulative and contain prior
continuation generations verbatim. Fresh distillation keeps only the latest
recognized generation, while inherited provenance fragments receive the same
non-mutating prompt projection. Detailed `Pending Tasks` narrative is retained
only for the latest authoritative task fragment; older or unattested task
sections remain recoverable from their immutable memory/CIR objects but do not
enter active context. Distinct sibling summaries are kept, and containment
deduplication applies only when one canonical provider summary contains the
other byte-for-byte. Opaque legacy digests without fragment provenance are not
generation-truncated.

## Synchronization

### `cxt push`

```text
cxt push [remote [branch]] [--append | --force]
```

Synchronizes local objects and selected refs to the specified remote (`origin` by default).
The reported `checked N ref(s)` counts the requested reconciliation set,
including retained-history refs. Unchanged remote refs may be omitted from the
request; this count is not the number of ref updates sent over the network.

- The default rejects a non-fast-forward update.
- `--append` preserves both histories by placing the local segment after the
  remote head through the graph overlay.
- `--force` replaces remote ref state and may make remote-only history
  unreachable from that ref.

Prefer `--append` or pull-and-retry over `--force`.

For a slow or failing push, enable optional diagnostics for that invocation:

```sh
CXT_SYNC_DIAGNOSTICS=1 cxt push
CXT_SYNC_DIAGNOSTICS=1 git push
```

When a recorded stage fails, stderr receives one `cxt sync diagnostics:` JSON
report, including when an append retry subsequently succeeds. Operations with
no recorded failures remain silent. The report distinguishes inventory from
document/chunk negotiation and records preparation, token lookup, request,
response-header and response-body timing, caller budget remaining, numeric
counts and process-local operation/request ordinals. An append retry shares
the same caller budget and collector. The report retains at most 64 events;
stage aggregates and the number of dropped events survive eviction.

Reports contain no request/response contents, credentials, addresses, identities,
file paths or raw errors. A request-write milestone does not prove that the
server processed or committed the request. `timeout_active` means a timeout
was observed while the caller context was still active; it does not identify
the server's internal cause. Diagnostics do not change cancellation, retries,
acknowledged progress, the hook's existing 60-second context deadline, or the
HTTP client's 30-second timeout. Synchronous token lookup and Git subprocesses
can still run beyond a context deadline; recording that deadline does not
turn it into a hard wall-clock limit.

### `cxt pull`

```text
cxt pull [remote [branch]]
```

On a connected clone's first pull, an empty worktree selection can be initialized
only when the authorized server history binds the fetched current branch tip to
the exact current Git commit. Branch identity, code, memory preparation and the
local selection are checked again before publication. An unrelated latest tip,
an orphan selection, or an existing historical cursor is never substituted.

Observes the named remote, reconciles permitted local pointers, then records the
selected-code context/memory application. The default remote is `origin`.

- The default keeps local state and reports diverged branches or causal memory
  forks instead of choosing a winner by arrival time.
- `--force` is rejected before mutation. Use `cxt repair --preview` and
  `cxt repair --apply` for a reviewed exact-pointer conflict resolution.
- If validated server metadata intentionally remains behind a local snapshot,
  cxt keeps a guarded local cursor so later pulls do not download that same
  projection forever. The cursor is only a disposable negotiation hint: a
  local or remote metadata change invalidates it, and it never participates in
  refs, reachability, or push state.

Review the old and new pointers in the repair plan before applying it.

## Tags and stashes

### `cxt tag`

```text
cxt tag
cxt tag <name> [<ref>]
```

Lists tags or creates an immutable tag. With no ref, tags the current head.
Tags are synchronized on the next push.

### `cxt stash`

```text
cxt stash
cxt stash push [-m <message>] [--provider <claude|codex>]
cxt stash list
cxt stash pop
```

Stores active context separately and restores the branch-head context. `pop`
restores and removes the newest context stash. Managed Git hooks mirror
ordinary `git stash` and `git stash pop` operations.

Session `stash pop` explicitly restores the original conversation, including
unpublished local work, using the existing provider replay path. It does not
use the default cloud memory-only input package. Replay may use the provider's
saved compaction state and bounded seed rules; the original archive stays
unchanged. Failure or a memory-only downgrade keeps the stash. The stack entry
is removed only after a resumable file is prepared and only if the whole stack
still matches the observed version. A concurrent push keeps both entries and
reports a conflict; preparation does not prove the provider resumed the file.

`stash push` uses the same provider selection rule as `cxt save`: explicit
`--provider`, then a verified live wrapper, then the newest capture-eligible
session.

## Team settings and secret masks

### `cxt settings`

```text
cxt settings pull
cxt settings list
cxt settings restore [index]
```

- `pull` applies available team `.claude/`, `.agents/`, and `.codex/` settings.
- `list` shows current setting-object hashes and local replacement backups.
- `restore` restores a backup; the default index is `0`.

`pull` skips only typed absence (`ErrNotFound`, including the server's 204
"not configured" response). Authorization, network, verification, and local
write errors stop the command with a nonzero exit and preserve their typed
cause. Bundles applied before a later failure remain applied; the error reports
that partial progress. Other HTTP failures, including an unclassified 404,
are not treated as an empty settings bundle.

### `cxt secrets`

```text
cxt secrets push [-p <passphrase>] [--remember] [--rotate]
cxt secrets pull [-p <passphrase>] [--remember] [--force]
```

Encrypts or decrypts the repository's `.cxtsecrets` list locally. The
passphrase is not sent to the server.

Passphrase lookup order:

1. `-p`;
2. `CXT_SECRETS_PASSPHRASE`; and
3. the per-repository credential saved by `--remember`.

The server returns an editing revision with each encrypted envelope. `pull`
records that revision, server/repository identity, passphrase fingerprint and a
hash of the downloaded plaintext in this worktree's `.cxt/secrets-baseline.json`.
`push` uses that original revision; it never fetches a newer one to bless an
already-edited file. A missing baseline permits first creation only when the
server confirms that no envelope exists. Upgrade the server and all clients
before writing; older clients without the precondition receive HTTP 428.

If another editor saves first, push fails and leaves your draft unchanged.
Copy the draft aside, pull the latest, compare and reapply your changes. A pull
refuses to overwrite unsaved local changes; `--force` explicitly permits that
replacement. A failed baseline write after a successful transfer requires
reloading before further edits. Do not delete the baseline to bypass a conflict.

`--rotate` performs a compare-and-swap passphrase rotation and rejects a stale
update. Share a rotated passphrase with authorized team members through a
separate secure channel.

The scrubber is defense in depth, not a credential-management system. Revoke
any secret that may have entered a session.

## Git hook management

### `cxt hooks`

```text
cxt hooks install
cxt hooks uninstall
```

Installs or removes the six managed Git hooks:

```text
post-commit
post-checkout
post-merge
pre-push
reference-transaction
post-rewrite
```

Existing user hooks are chained and restored on uninstall.
Installation requires an initialized store (`cxt init` or `cxt setup`) and
repairs both `.gitignore` and `.git/info/exclude` before writing hook scripts.

After `git pull` or merge, the post-merge hook first fetches new team context
without moving the active local context ref. A merged branch context is then
losslessly appended to the base timeline; the local context ref converges only
when that preserves its history. This does not replace, slice, or copy another
conversation into the running agent session. The next prompt instead receives
one terminal-scoped notice containing only the validated incoming snapshot
IDs. Its range is calculated from a durable pre-promotion baseline, so a local
PR promotion cannot hide its own delta and a failed delivery remains retryable.
Teammate-authored commit labels, author fields, and conversation text are not
copied into the model's `additionalContext`; inspect those untrusted details in
the web context view. The notice is consumed once, expires after 24 hours,
keeps the newest 12 visible snapshots, and remains capped at 4 KiB.

## Configuration

### `cxt config`

```text
cxt config <key>
cxt config <key> <value>
```

Reading a key prints its effective local value. Supported keys are:

| Key | Values | Default | Effect |
|---|---|---|---|
| `checkout.mode` | `auto`, `prepare` | `auto` | Restore automatically or only prepare the resume action after Git checkout |
| `load.mode` | `full`, `reconstructed`, `memory`, `default` | structured memory | Retained for compatibility; does not override managed latest-server-main input. Archive replay requires explicit `--mode` per command; `default` clears the stored preference. |
| `boundary.enforce` | `kill`, `none`, `default` | `kill` | Managed fresh wrappers prepare and validate the next input before stopping their current child; failed preparation preserves it. Legacy native-resume wrappers use the prepared-seed restart path. Unmanaged app sessions stay open and receive a bounded handoff. |
| `capture.debounce` | non-negative seconds, `default` | 60 seconds | Minimum interval for repeated Stop-event captures |
| `secrets.scrub` | `off`, `standard`, `strict`, `default` | `standard` | Pattern-based scrub tier |
| `secrets.redact` | replacement text, `default` | built-in redaction token | Exact-secret replacement text |
| `secrets.minlen` | `0` through `64` | `4` | Minimum exact-secret length; `0` restores the default |

Examples:

```bash
cxt config checkout.mode prepare
cxt config load.mode default
cxt config secrets.scrub strict
cxt config capture.debounce 120
```

Configuration is repository-local.

## Maintenance and diagnostics

### `cxt fsck`

```text
cxt fsck
```

Runs a read-only server audit for reachability, roots, unreachable snapshots,
and missing parents. An unreachable snapshot remains stored but is not
referenced by a current ref or pending session; this can include superseded
captures or intentionally detached history. A missing parent is the structural
integrity error. Requires a configured remote.

### `cxt reflog`

```text
cxt reflog
```

Lists server ref movements newest first. Requires a configured remote.

### `cxt repack`

```text
cxt repack
```

Repackages legacy local documents into chunked content-addressed storage and
reports reclaimed duplicate-prefix bytes. Snapshot identity remains unchanged.
Back up important alpha data before storage maintenance.

## Global information

```text
cxt version
cxt --version
cxt help
cxt -h
cxt --help
```

`version` prints the release version. All help aliases print the public command
catalog and return success.

## Environment variables

| Variable | Purpose |
|---|---|
| `CXT_REMOTE` | API base fallback when no repository remote is configured |
| `CXT_TOKEN` | Non-interactive authentication token |
| `CXT_SYNC_DIAGNOSTICS=1` | Optional bounded failure diagnostics for explicit push and Git pre-push |
| `CXT_NAME`, `CXT_EMAIL`, `CXT_TEAM` | Snapshot author identity overrides |
| `CXT_NO_BROWSER=1` | Prevent automatic browser launch during login |
| `CXT_SECRETS_PASSPHRASE` | Passphrase fallback for encrypted secret-mask sharing |
| `CXT_KEEP_SESSION=1` | Suppress automatic session switching during a Git branch transition |
| `CXT_CARRY=1` | Carry the active session across a context transition |

Do not expose tokens, passphrases, or private session content in command output,
shell history, issue reports, or CI logs.

### Repository branch-history compatibility

Update and sync all CLI replicas before enabling **Branch history protection**
in repository settings. Once enabled, older clients cannot mutate branch refs.
New clients send the persisted branch identity and publish verified birth,
rename and archive history before refs. A same-name/same-hash identity conflict
requires reconciliation; force-push does not override branch identity.

`.cxt/refs/heads/*` may contain a JSON ref with `branch_id`, not just a hash.
Use `cxt log`, `cxt branch list`, or `cxt doctor` instead of interpreting the
replica's internal files as a public interface. Existing plaintext refs remain
readable; only a first proven cloud identity can be adopted automatically.

### PR discovery and delivery recovery

Git merge ranges are durable discovery records. Discovery queues each verified
PR request before considering the range delivered. Server acceptance transfers
retry responsibility; it does not mean context or memory is already merged.
Offline requests remain in the local delivery queue and retry on synchronization.
Discovery and delivery rotate by their last durable attempt, so an older failing
request cannot monopolize the per-run budget. Corrupt discovery records remain
available for diagnosis while independent valid ranges continue. Exact source
publication, Git order and idempotent completion remain server responsibilities.

### Interrupted synchronization

Pull stages each verified document before proceeding to the next. Verified
chunks are also persisted before a large document finishes, so restarting the
client can reuse progress after a network failure. Snapshot/ref publication waits
until the entire selected pull passes integrity checks. Staged bodies are not
published partial history. Unreferenced staging may later be garbage-collected;
a retry then downloads it again safely.

Foreground Git hooks replay durable branch operations for the current symbolic
Git ref. Unrelated pending operations remain in the journal for background replay;
they no longer make an ordinary current-branch hook restore another branch's
context first. Existing hook deadlines are unchanged. Full explicit push/pull
still synchronize retained history and may take longer on first synchronization;
this change does not claim constant-time synchronization of a large archive.

Git reference transactions describe both logical changes and storage maintenance.
A zero-old-OID event for an already existing branch at the same target is packed
storage creation, not branch birth. It must not create a context branch or enter
the replay writer lock. Terminal callbacks without a matching durable vote do
not schedule a replay. Real `git pack-refs --all` and a contended-journal fixture
cover this distinction; genuine new branches retain the fail-closed birth vote.


## Capture recovery

`cxt capture list [--all] [--json]` inspects retained commit-capture attempts across
all worktrees, without network access, live capture, automatic replay or changing
refs. `show <id>` displays recorded outcomes and an expected fingerprint. Doctor
also reports unresolved attempts; acknowledged gaps remain visible in its JSON.

| State | Meaning | Action |
| --- | --- | --- |
| `needs-review` | A saved output or completion proof is missing | Inspect original evidence; never substitute today's transcript |
| `ready-to-retry` | Every provider outcome was recorded | Retry exact finalization; immutable observations are still required |
| `ready-to-publish` | Completion is durable; publication is missing | Retry publication |
| `superseded` | A later completed publication covers this attempt at the same code/worktree/branch position | Record the verified replacement |
| `acknowledged-gap` | An operator explicitly acknowledged unprovable missing capture | Preserve the gap and its reason |
| `completed` | This attempt has its own accepted publication (or no context to publish) | No recovery needed |

```sh
cxt capture show <id>
cxt capture retry <id> --expect <fingerprint>
cxt capture resolve <id> --expect <fingerprint>
cxt capture acknowledge <id> --expect <fingerprint> --reason "Evidence unavailable after investigation"
```

`resolve` requires the same repository, branch generation, local branch, worktree
and Git SHA, a later completed attempt, matching provider/session evidence, a
publication receipt and coverage of the original baseline and saved outputs.
Inspection alone does not suppress retries. An immutable resolution sidecar records
the original and replacement fingerprints; journal writers serialize and compare
expected state. Stale reviews or conflicting decisions fail. Original attempts
and their completion bits are never rewritten by resolve/acknowledge.

`acknowledge` is not successful recovery and creates no publication. It retains
an auditable gap and stops repeating an impossible automatic retry. `retry` never
rescans the current provider; successful local publication still needs `cxt push`
for delivery. These are local replica diagnostics, not server completion receipts.

## Machine failures and upgrade notes

Commands advertising `--json` emit version-1 failure envelopes on stderr. Success
stays on stdout. Automation should use `error.code` and `exit_code`, not translated
messages. Exit codes are 2 for argument errors, 3 for selection/conflict/index
conditions, 4 for authorization, 5 for integrity/unsupported storage versions,
6 for input preparation limits, 130 for cancellation, and 1 for other failures.
`mutation_state: inspect_receipts` means a resumable operation may have completed
partly; it is not a rollback claim. Preflight failures report `unchanged`.

Precise causes are emitted only when the operation identifies them through a
typed error; message text is never used to guess a machine code:

| `error.code` | Exit | Meaning |
| --- | --- | --- |
| `index_changed` | 3 | An explicit staging `--expect` revision differs from the observed index. Re-read/review the index before trying again. |
| `code_position_mismatch` | 3 | Staging, selected pull, or prepared input delivery observed a different Git commit from its selected commit. Prepare against the current code before retrying. |
| `invalid_ref` | 2 | A typed reference validation/resolution failure reached command execution, such as an ambiguous branch/tag. |
| `delivery_failed` | 1 | Prepared input encoding/materialization, launch validation/start, or a required delivery receipt failed. `phase` is `delivery`; provider acceptance and rollback remain unproven. |
| `position_changed` | 3 | A broader context/branch/server selection changed without an identified commit mismatch. |
| `conflict` | 3 | A typed conflict without a more specific cause, including sync ref rejection. |

An index CAS failure without an exact typed cause can still be `conflict`; a
general selection failure stays `position_changed`. Plain errors remain
`needs_attention`, even if their text contains one of these code names.
Cancellation, timeouts, integrity failures, and preparation limits retain their
existing codes through delivery wrappers. A provider's later nonzero task exit
is not proof that input delivery failed. `invalid_ref` reported during command
execution retains `mutation_state: inspect_receipts`; only argument preflight
can promise `unchanged`. Commands without a `--json` contract use the same exit
classification and human-readable stderr, including provider-owned flags.

Index, commit, pull and delivery receipts are additive versioned records. Unknown
index versions fail closed. Raw documents, natural parents, branch IDs and older
memories are preserved. Older binaries do not understand frozen staging; do not
use them to commit a repository with an active new index. Before downgrade,
finish or explicitly unstage the index and keep its archives/receipts. A new CLI
cannot make an independently invoked old binary honor a new local contract.
