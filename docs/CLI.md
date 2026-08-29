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
- A workspace remote uses
  `https://<host>/<username>/<workspace>` with exactly two path segments.
- `<ref>` accepts `HEAD`, a branch name, a tag name, or a full
  `sha256:<64-hex-character>` snapshot ID. An omitted ref resolves to the
  current context head where supported.
- Providers are `claude` and `codex`.
- Restore modes are:
  - `full`: materialize the native session when possible;
  - `reconstructed`: rebuild a provider-compatible session from normalized
    context; and
  - `memory`: inject the distilled memory representation.
- Git and provider hooks are fail-open: a capture or sync failure is reported
  but does not block the Git or agent operation.
- Every public command supports `-h` and `--help`. Help and usage errors are
  resolved before cxt creates adapters, contacts a remote, or changes local
  context state. Unknown flags and missing flag values are rejected. Value
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

## Recommended onboarding

### `cxt setup`

```text
cxt setup [remote-url] [--no-login]
```

Runs the complete, idempotent onboarding sequence:

1. initialize the local `.cxt` store;
2. install managed Git hooks;
3. register the workspace remote when supplied;
4. authenticate through the browser device flow unless `--no-login` is set;
5. merge Claude Code and Codex lifecycle hooks; and
6. pull team settings when authenticated.

Example:

```bash
cxt setup https://cxthub.example/alice/platform
```

Rerunning the command reports and repairs missing setup steps without replacing
an existing remote that points somewhere else.

## Repository and authentication

### `cxt init`

```text
cxt init [--no-hooks] [--remote <workspace-url>]
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
cxt repo create <workspace-url>
```

Convenience alias for initialization plus `origin` registration.

### `cxt remote`

```text
cxt remote [-v]
cxt remote add <name> <workspace-url>
cxt remote remove <name>
```

The `origin` URL determines both the server API endpoint and the content
repository identity.

```bash
cxt remote add origin https://cxthub.example/alice/platform
cxt remote -v
cxt remote remove origin
```

Changing an existing remote requires removing it and adding the replacement.

### `cxt login`

```text
cxt login [token]
cxt login -t <token>
```

Without a token, starts the browser device flow for the configured `origin`.
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

Runs Claude Code with branch-context seeding and passes remaining arguments to
the installed `claude` executable.

### `cxt codex`

```text
cxt codex [codex-arguments...]
```

Runs Codex with branch-context seeding and passes remaining arguments to the
installed `codex` executable.

### `cxt mcp`

```text
cxt mcp
```

Starts the read-only MCP server on standard input/output. It exposes:

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

## Capture and commit commands

### `cxt add`

```text
cxt add [claude|codex]...
cxt add .
```

Stages the providers to capture on the next `cxt commit` or Git post-commit
hook. With no provider, or with `.`, both providers are staged. The staging
selection is cleared after a successful commit capture.

### `cxt commit`

```text
cxt commit [-m <message>]
```

Captures active sessions from the staged providers, or from both providers
when nothing is staged. Each successful capture is distilled into reusable
memory. The command also schedules synchronization that resolves matching
remote pending-session pointers.

Ordinary `git commit` invokes the same capture path and adds the Git commit
message and short SHA link automatically.

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
cxt list [--branch <branch>]
cxt log [--branch <branch>]
```

Lists local snapshots, optionally restricted to a branch. `log` is an alias for
`list`.

## Restore and branch commands

### `cxt checkout`

```text
cxt checkout [<ref>] [-b <new-branch>]
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

Restores a ref. With `-b`, creates and restores a new context branch from that
ref. Git checkout hooks normally invoke the corresponding behavior
automatically. A new branch seed carries the main head's compact memory plus
the departure branch head's session conversation. Conversations that fit are
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

Creates a context branch from a specific ref and restores it.

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
event, recreates the context pointer, and restores the provider session. A
stale client cannot recreate an archived pointer by an ordinary push; it must
observe or explicitly create the newer active generation.

### `cxt load`

```text
cxt load [<ref>]
  [--provider <claude|codex>]
  [--mode <full|reconstructed|memory>]
```

Restores a snapshot without creating a branch. Omitting `<ref>` loads the
current context head. `--provider` enables supported cross-provider
materialization. If an oversized conversation must be trimmed, cxt first
distills the exact omitted span and fails without creating a provider session
file if that projection is unavailable; it never resumes from an unexplained
recent tail. A new full or reconstructed provider session also receives the
bounded portable-memory projection when the conversation itself fits. The
current snapshot's conversation delta and exact ancestor user/assistant items
already present in the replay are excluded because those turns follow
verbatim; conversation from sibling/team lineages that is absent from the raw
replay remains portable. An exact provider compaction summary already present
in the replay is not repeated. cxt trims at a user-turn boundary only when the
combined verbatim conversation and portable projection exceed the provider's
seed budget.

The mode priority is:

1. the command's `--mode`;
2. local `config load.mode`;
3. the authenticated account preference from the server; and
4. `full`.

`memory` keeps the full immutable digest in cxt storage but injects at most a
64 KiB projection into the target provider's instruction file. cxt appends or
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
cxt push [--append | --force]
```

Synchronizes local objects and refs to `origin`.

- The default rejects a non-fast-forward update.
- `--append` preserves both histories by placing the local segment after the
  remote head through the graph overlay.
- `--force` replaces remote ref state and may make remote-only history
  unreachable from that ref.

Prefer `--append` or pull-and-retry over `--force`.

### `cxt pull`

```text
cxt pull [--force]
```

Downloads objects and refs from `origin`.

- The default keeps local state and reports diverged branches or causal memory
  forks instead of choosing a winner by arrival time.
- `--force` adopts remote ref state and resolves a memory fork by selecting the
  remote memory pointer. The losing immutable local memory object and raw
  session remain stored; run `cxt memorize` again to project that session onto
  the selected remote lineage.
- If validated server metadata intentionally remains behind a local snapshot,
  cxt keeps a guarded local cursor so later pulls do not download that same
  projection forever. The cursor is only a disposable negotiation hint: a
  local or remote metadata change invalidates it, and it never participates in
  refs, reachability, or push state.

Review local work before using `--force`.

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

### `cxt secrets`

```text
cxt secrets push [-p <passphrase>] [--remember] [--rotate]
cxt secrets pull [-p <passphrase>] [--remember]
```

Encrypts or decrypts the repository's `.cxtsecrets` list locally. The
passphrase is not sent to the server.

Passphrase lookup order:

1. `-p`;
2. `CXT_SECRETS_PASSPHRASE`; and
3. the per-repository credential saved by `--remember`.

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
| `load.mode` | `full`, `reconstructed`, `memory`, `default` | `full` | Default restore fidelity; `default` clears the local override |
| `boundary.enforce` | `kill`, `none`, `default` | `kill` | Enforce process isolation across context switches when a live owning `cxt claude`/`cxt codex` wrapper can restart the prepared seed; unmanaged sessions are not killed and receive an explicit resume command |
| `capture.debounce` | non-negative seconds, `default` | 60 seconds | Minimum interval for repeated Stop-event captures |
| `secrets.scrub` | `off`, `standard`, `strict`, `default` | `standard` | Pattern-based scrub tier |
| `secrets.redact` | replacement text, `default` | built-in redaction token | Exact-secret replacement text |
| `secrets.minlen` | `0` through `64` | `4` | Minimum exact-secret length; `0` restores the default |

Examples:

```bash
cxt config checkout.mode prepare
cxt config load.mode reconstructed
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
and missing parents. Requires a configured remote.

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
| `CXT_NAME`, `CXT_EMAIL`, `CXT_TEAM` | Snapshot author identity overrides |
| `CXT_NO_BROWSER=1` | Prevent automatic browser launch during login |
| `CXT_SECRETS_PASSPHRASE` | Passphrase fallback for encrypted secret-mask sharing |
| `CXT_KEEP_SESSION=1` | Suppress automatic session switching during a Git branch transition |
| `CXT_CARRY=1` | Carry the active session across a context transition |

Do not expose tokens, passphrases, or private session content in command output,
shell history, issue reports, or CI logs.
