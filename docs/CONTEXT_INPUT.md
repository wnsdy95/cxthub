# Context selection and agent input

CXTHub keeps the original conversation archive in the server repository. An
agent input package is a bounded, disposable projection of that archive and its
code-scoped memory. It is not a new conversation ancestor or a replacement for
the archive. Physical content-addressed chunks, logical conversation ranges,
Git publication bindings, and provider input budgets are separate concepts.

## Selection and provenance

The server context query determines snapshot inclusion at a branch/code
position. Historical PR completion and current code inclusion remain separate
facts. A later mutable overlay cannot by itself make a future PR part of an
earlier code position. CLI history reads retain the server revision and state
hash; local-only history identifies itself as an observation rather than an
authorization or server acknowledgement.

The optional `segment_limit`, `segment_offset` and `segment_state_hash` query
parameters return logical conversation ranges in the same snapshot order.
Continuation requires the original selection hash. Each page contains only its
snapshot metadata. A finalized publication can establish an exact same-session
natural-parent prefix. Otherwise the response explicitly describes a full
source or unavailable coverage; equal text and branch labels are not proofs.

## Default memory input

Server memory projection distinguishes archived read volume from retained
contributions. Cumulative archives may repeat the same earlier fragments;
reading more than 64 MiB across those objects does not by itself reject a
projection. The request retains at most 8 MiB of serialized-weight body cache
and composes an ordered union using verified provenance and exact fragment
identity. Evicted contribution bodies are hash-validated when loaded again.

Individual objects, retained unique contributions and final wire output remain
bounded at 64 MiB, with separate snapshot, traversal and entry bounds. These are
logical payload limits, not a guarantee of 64 MiB process heap usage. Resource
exhaustion returns HTTP 422 `memory_projection_limit`; the server neither
truncates the result nor changes the archived memory. This server bound is
separate from the agent's token budget below.

An unqualified managed fresh launch and unqualified `load` prepare a versioned
package containing:

- Server-selected project memory and its application state.
- Explicitly selected personal work state and exact user constraints, when
  supplied. Another contributor's task list is never selected automatically.
- The repository, code position, source pointers, revision and coverage gaps.

The default allowance is 8,000 conservative UTF-8 byte units. This is an
engineering bound, not a measured optimal model window or quality result.
Required personal conditions are not silently truncated to meet that bound.
Use `--work-state <file>` to select a scoped personal handoff explicitly; see
[personal work format and validation](PERSONAL_WORK.md).
An ordinary current selection queries the server's integrated memory for its
selected branch, snapshot and Git commit. A historical selection keeps its exact
memory revision (including an explicitly empty revision), even after it receives
a branch name. The local recovery cursor's `memory_pinned` marker alone does not
disable current branch integration. Preparing input never changes that cursor
or an applied-pull receipt; those receipts record explicit synchronization, not
the provider's current input cache.
Synthetic input packages are excluded from subsequent seed reconstruction and
memory distillation, including summaries that quote a nested package.

Desktop branch notices use the same prepared package for a selected snapshot.
They leave the provider-owned conversation open and keep the existing 16 KiB
handoff limit. An orphan root has no selected conversation: its explicitly
pinned inherited project memory remains a separate memory-only handoff.
Preparing a notice or starting a process does not prove that an app accepted it.

A first managed default CLI launch can start a repository with no context yet.
This is a distinct **verified-empty bootstrap**, not a fallback after a failed
context read. It requires a pristine local context replica and an authorized,
unfiltered `scope=all` server query with empty snapshot **and history** arrays,
including unreachable captures. Missing fields, missing repositories (404),
denied access (403), transport failures, and nonempty server data stop the launch.
Explicit load/ref requests, history input, personal work-state input, native
resume, and automatic branch-transition restarts do not use bootstrap.

The bootstrap materializes a small provenance notice in a new provider session;
it does not create a context snapshot, memory object, or branch ref. It rechecks
the exact server state hash and graph/evidence/pending revision, actual Git
branch/code, and local worktree selection before launch. A symbolic Git branch
whose ref genuinely does not exist is recorded as `unborn: true` with no commit
hash; other Git failures cannot stand in for an unborn repository. A first Git
commit or branch switch during preparation invalidates that proof.

Bootstrap artifacts identify their kind as `verified_empty_repository`.
Delivery receipts use `mode: empty_bootstrap` and include the server revision,
worktree hash, branch, and actual commit or explicit unborn state. `prepared`
and `launched` remain separate, and acceptance stays `unknown`. These checks
observe the selected revision; they do not lock Git, the server, and a provider
process into one transaction.

Managed fresh CLI wrappers advertise the `prepare-first-v1` transition protocol.
Their Git hook queues a transition without renaming live sessions or killing the
child. The wrapper prepares and validates the next input while that child is
alive, then reuses the prepared input for the restart. A failed preparation keeps
the current child and does not retry repeatedly for the same boundary. A later
transition may retry. The wrapper retires only its own old session after stopping
it; other terminals stay open. Legacy native-resume wrappers keep their explicit
replay path. Upgrade long-running wrappers as well as the CLI binary.

## Explicit historical input

```sh
cxt --pull --context-budget 200k codex --yolo
cxt --pull --context-budget full codex --yolo
cxt --pull codex --yolo
```

The last two requests both ask for 800,000 tokens: `full` is the original
requested budget, not a promise of 800,000 tokens of input. Budgets can be chosen
in 100k steps (`100k`, `200k`, through `800k`); `k` means 1,000 tokens. Smaller
positive token counts are also accepted. History is quoted evidence in a new
package, not decoded opaque reasoning or native state replay.
Native resume, help and noninteractive provider commands preserve their own
argument and session semantics; incompatible context prefixes fail.

For a verified runtime, preparation resolves the actual model and its context
window `W` before selecting history. Total initial input must be at most
`floor(0.8 * W)`, including host system instructions, tools, prompt and other
fixed host input, package framing, project memory, exact personal constraints,
provenance and selected history. Required output/reasoning/work capacity is
reserved separately: initial input is also limited by `W - reserved_tokens`.
It is not deducted again from the twenty percent already left outside the
80-percent input limit. A known automatic-compaction trigger further caps total
initial input strictly below that trigger.

The effective package budget is the smaller of the original request and the
remaining initial-input capacity after host input and framing. The package keeps
the original request in `policy.budget_tokens`; its optional `budget` records the
resolved provider/model, host version, tokenizer, requested/effective limits,
window, host input, framing, output reserve, compaction threshold and adjustment
reason. Launch summaries show the effective limit and any adjustment. An
explicit provider `--model` must match this accounting; without that option,
accounting uses the runtime's resolved model. A model/window override alone is
not verified host or tokenizer evidence.

The cloud reader requests complete newest-first turns from
`GET /repos/{repoID}/docs/{hash}/turns`. Each page is separately authorized and
read from server-verified event indexes and chunks. It is bounded by turn count
and at most 4 MiB of event JSON. The response's turn hashes validate its wire
bodies; they are not an independent proof of the full document hash. An exact
same-provider/session prefix proof can skip an older cumulative source.

The package builder selects the latest complete turns that fit the effective
budget, stops reading when the next complete turn exceeds it, keeps tool calls
and results together, and renders selected turns in chronological order.
Mandatory memory, exact personal constraints and provenance must fit first;
constraints are never shortened. If mandatory content or the newest complete
turn cannot fit, the explicit history request fails without a quiet memory-only
fallback. A turn exceeding the transfer byte bound is reported separately from
token capacity. No source archive is shortened. Context and memory
authorization/revisions are checked again before materialization.

If the server revision or verified runtime limits change while preparing input,
the reader makes at most three complete attempts in total. Changed runtime
limits discard the old selection and reselect recent turns with the new budget.
It fixes the original snapshot, branch, code and
context content across those attempts, discards partial pages, and reauthorizes
the reads. An observed selection change, denied access or invalid source stops
immediately. Exhausted server-revision contention returns `position_changed`;
exhausted runtime-limit/model contention returns `provider_capability_unknown`. Explicit
personal work does not use this retry because its imported provenance must be
checked again by a new invocation. A typed `memory_cursor_stale` response for a
continuation page restarts within that same attempt budget. Generic conflicts
and cursor errors on the initial page remain terminal. Older servers returning
a generic conflict require a new invocation.

Strict native history delivery requires verified host/model capacity, known host
input, an exact tokenizer, framing/output reservations and a known compaction
threshold. Before preparation and launch receipts are recorded, delivery checks
that budget accounting reproduces the original request, provider, model and
exact selected-token count within the effective limit. **This build does not yet
ship a verified native combination**, so the three launch examples above
currently return `provider_capability_unknown` before starting the provider.
Adaptive accounting does not enable an unverified runtime or invent provider
acceptance.

The preparatory [native Codex transport](NATIVE_HOST_TRANSPORT.md) has a separate
owned-process and injection-acknowledgement contract. It remains unwired from
the launch path until the runtime capability and interactive handoff gates pass.

Inspectable artifacts are available without claiming native acceptance:

```sh
cxt load --provider codex --context-budget 200k --output context.json
cxt load --provider codex --context-budget full --output context-full.json
```

This separate artifact path uses the explicitly inexact byte counter and the
requested budget, preserves selection and source provenance, and cannot be
relabelled as launchable by changing one field. Prepared and launched delivery
receipts preserve a copy of verified budget accounting when present; memory and
empty-bootstrap receipts omit it. Acceptance stays unknown without provider
evidence. Current-context mutation is never part of `load`.

## Local state and compatibility

`add` freezes exact source ranges in a worktree-scoped versioned index. A manual
`commit` consumes that frozen generation and records exact source observations,
publication events and a recoverable local operation. New dialogue after `add`
is not silently included. Git hooks capture independently and do not consume
the manual index. Staged sources and unfinished operations participate in the
object-retention boundary used by current readers and collectors.

Session `stash pop` is an explicit original-session recovery operation, separate
from a fresh memory-only launch. It restores local unpublished work through the
provider replay path and retains the source archive. It acknowledges the stash
only after preparing a resumable conversation and comparing the entire stack;
failed preparation, a memory-only downgrade or a concurrent stack change keeps
the entries. Staged stash restores the frozen index instead.

`fetch` stores remote observations and immutable objects without applying
current refs or memory. `pull` additionally reconciles selected state. Applied
projection receipts identify the repository, remote, worktree and code; status
marks a receipt stale after a different selection. A receipt proves a past
authorized operation, not current server permission.

An immediate `pull` retries preparation/application up to three times when only
server read revisions move. Its original checkout state, frozen-index generation
and previous receipt stay fixed; every attempt rereads and reauthorizes all
pages. Effective-memory state hashes include repository revisions, so the
reader also pins the actual page contents and lineage. Changed contents, local
selection/index movement, malformed pages, denied access and a lost local CAS
stop the operation. A separately previewed plan still requires its exact read
revision when applied; it is never silently replaced with a newer plan.

Explicit remote/ref requests cannot replay unrelated PR promotion jobs. Tokens
from one configured server are not sent to another; saved host credentials are
also withheld from non-HTTPS destinations other than explicit loopback
development endpoints. Forced pull replacement uses a preview/expect repair
plan and preserves displaced records instead of silently overwriting them.

Upgrade CLI processes and installed hooks together. The legacy provider-only
`staged` setting cannot be migrated into frozen source bytes; run `cxt add`
again. Unknown index versions fail without rewriting them. An old binary that
does not implement the new index contract cannot be made safe by a new file
alone: do not run it or its collector alongside new staged work. Preserve the
replica and use a compatible reader during rollback. These local journals are
not a distributed ACID transaction spanning Git, provider files and the server.

See [CLI commands](CLI.md), [history](CONTEXT_HISTORY.md), and
[transaction boundaries](COLLABORATION_TRANSACTIONS.md). Real host acceptance,
repeat-handoff model-quality evaluation and production cloud load measurements
remain separate release validation gates; synthetic fixtures do not satisfy
those gates.
