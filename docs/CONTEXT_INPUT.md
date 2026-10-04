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

Every managed fresh context injection uses the latest authorized **server
`main`**: default memory input, explicit budgeted history, `load` delivery,
branch seeds and desktop handoffs. The local branch, worktree, local `main` ref
and rewound memory cursor do not select that input. The server resolves `main`
directly; local aliases, refs and the local Git commit cannot pin its tip.

The package's source selection `branch`, `snapshot_id` and `code_commit`
identify server `main` and its observed tip/code. The additive, optional
`source_policy: latest_server_main` and `working_position` fields distinguish
that source from the actual branch and code being edited. `working_position`
records the actual Git branch and commit separately; a feature worktree is not
relabeled as `main`. Its state still participates in delivery validation.

Local `main` is never authoritative. Missing or denied server `main`, invalid
source data and failed memory reads stop managed injection without falling back
to local context. A missing `main` in a nonempty server repository is not an
empty-repository bootstrap. Historical archive reads, including
`load <ref> --output`, retain their explicit source and memory revision; they
do not deliver a managed fresh package.

Before delivery, cxt reauthorizes and revalidates the source and memory against
the prepared selection, and checks the separate working position. Concurrent
source, memory or worktree changes invalidate the package: delivery aborts or
the input must be prepared again within the applicable retry policy. These are
bounded observations, not a distributed transaction or a lock across the server,
Git and provider process.

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

An unqualified managed fresh launch and prepared `load` delivery use the latest
authorized server `main` to prepare a versioned package containing:

- Server `main` project memory and its application state.
- Explicitly selected personal work state and exact user constraints, when
  supplied. Another contributor's task list is never selected automatically.
- The repository, main source/code pointers, separate working position,
  revision and coverage gaps.

Stored `load.mode` and account server preferences remain for compatibility but
cannot override managed input. Archive replay requires explicit `--mode` on
each command; Git hooks always use structured managed input. When managed
`load` omits `--provider`, it can infer the provider from authorized server main
snapshot metadata and record it in the package's optional `provider` field,
without requiring a local archive.

The default allowance is 8,000 units: local text tokens when a documented model
mapping is available, otherwise explicitly inexact UTF-8 byte units. This is an
engineering bound, not a measured optimal model window or quality result.
Required personal conditions are not silently truncated to meet that bound.
Use `--work-state <file>` to select a scoped personal handoff explicitly; see
[personal work format and validation](PERSONAL_WORK.md).
Managed input queries the server's integrated memory for the latest authorized
`main` snapshot and its server-selected code commit. A historical or rewound
local cursor, including `memory_pinned`, does not pin managed input to historical
memory. Historical archive inspection retains its exact memory revision,
including an explicitly empty revision. Preparing input never changes that
cursor or an applied-pull receipt; those receipts record explicit
synchronization, not the provider's current input cache.
Synthetic input packages are excluded from subsequent seed reconstruction and
memory distillation, including summaries that quote a nested package.

Desktop branch notices and managed branch seeds use the same latest authorized
server `main` input rule. Notices leave the provider-owned conversation open and
keep the existing 16 KiB handoff limit. Branch creation still derives natural
parents and archival memory from its actual creation source. An orphan root retains its
separate inherited archival memory and has no natural conversation parent.
Input from main does not rewrite that ancestry or archival memory, and an
orphan cursor cannot substitute inherited memory for the authorized main input.
Preparing a notice or starting a process does not prove that an app accepted it.

A first managed default CLI launch can start a repository with no context yet.
This is a distinct **verified-empty bootstrap**, not a fallback after a failed
main/context read or a way to invent `main`. It requires a pristine local
context replica and an authorized, unfiltered `scope=all` server query with
empty snapshot **and history** arrays,
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
it; other terminals stay open. Existing native resume does not reinject context.
Explicit legacy replay restores the selected archive; it is separate from
prepared fresh injection. Legacy native-resume wrappers retain that explicit
replay path. Upgrade long-running wrappers as well as the CLI binary.

## Explicit budgeted history input

```sh
cxt --pull --context-budget 200k codex --yolo
cxt --pull --context-budget full codex --yolo
cxt --pull codex --yolo
```

The last two requests both ask for 800,000 tokens: `full` is the original
requested budget, not a promise of 800,000 tokens of input. Budgets can be chosen
in 100k steps (`100k`, `200k`, through `800k`); `k` means 1,000 tokens. Smaller
positive token counts are also accepted. Managed history input selects evidence
from the latest authorized server `main`, regardless of the working branch or
rewound cursor. History is quoted evidence in a new package, not decoded opaque
reasoning or native state replay.
Native resume, help and noninteractive provider commands preserve their own
argument and session semantics; incompatible context prefixes fail.

For a verified runtime, preparation resolves the actual model and its context
window `W` before selecting history. Budgeted initial input is limited to
`floor(0.8 * W)`, including host system instructions, tools, prompt and other
fixed host input, package framing, project memory, exact personal constraints,
provenance and selected history. Required output/reasoning/work capacity is
reserved separately: initial input is also limited by `W - reserved_tokens`.
It is not deducted again from the twenty percent already left outside the
80-percent input limit. A known automatic-compaction trigger further caps total
initial input strictly below that trigger.

The effective package budget is the smaller of the original request and the
remaining initial-input capacity after host input, framing, an internal-input
allowance and the initial user
task supplied in provider arguments. The task is counted as one complete text
under the resolved model's tokenizer, before selecting history; it is not trimmed
or split. Host input must exclude that separately reserved user message, and
the internal-input allowance also covers unknown native framing. The package keeps
the original request in `policy.budget_tokens`; its optional `budget` records the
resolved provider/model, host version, tokenizer, requested/effective limits,
window, host input, framing, optional `initial_prompt_tokens`, output reserve,
compaction threshold and adjustment
reason. Launch summaries show the effective limit and any adjustment. An
explicit provider `--model` must match this accounting; without that option,
accounting uses the runtime's resolved model. A model/window override alone is
not verified host or tokenizer evidence.

The initial task remains private invocation input. It is never copied into the
shared-memory package, its sources or serialized receipts. An in-memory
reservation binds its provider-normalized bytes and presence to the provider, resolved model,
tokenizer and count; equal counts do not authorize a substituted question.
Materialization and child launch reject a missing or mismatched reservation.
An explicitly empty task differs from no task, and multiple ambiguous positional
tasks are rejected rather than joined. Wrapper restart removes the original task
and prepares a new reservation. Model changes during preparation also recount it.
Codex CRLF and CR are normalized to LF before counting, matching native initial
TUI/resume submission. Raw argv remains unchanged; Claude text is not normalized
by this Codex-specific rule. This covers the supplied argv text; later interactive input and multimodal
attachments require their own native accounting. Default memory, archive
artifacts, bootstrap and existing native resume retain their existing behavior.

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
Each preparation fixes its observed server main snapshot, branch, code and
context content across those attempts, discards partial pages, and reauthorizes
the reads. Main tip/content or working-position movement requires a new
preparation; denied access or invalid source stops immediately. Historical
artifact requests instead retain their explicitly selected archive source.
Exhausted server-revision contention returns `position_changed`;
exhausted runtime-limit/model contention returns `provider_capability_unknown`. Explicit
personal work does not use this retry because its imported provenance must be
checked again by a new invocation. A typed `memory_cursor_stale` response for a
continuation page restarts within that same attempt budget. Generic conflicts
and cursor errors on the initial page remain terminal. Older servers returning
a generic conflict require a new invocation.

The approved `measured_reserve_v1` policy requires verified runtime/model window
and an exact supported local text tokenizer, but does not require advance
knowledge of all hidden system/tool input or the compaction threshold. Unknown
host input and unknown compaction are labeled separately from measured zero.
Known lower compaction thresholds still apply. The runtime scope must bind
provider/account routing, effective configuration, instructions and tools;
a model name or arbitrary window override is insufficient.

The initial extra allowance is `max(16000, floor(W / 20))` tokens, reserved
**inside** the 80-percent input limit. This is a conservative engineering policy,
not a guarantee of the hidden payload size. For a 1M window, a 2k first question,
no separately known overhead and no observations, full requests can retain up to
748k package tokens; complete-turn boundaries may retain less.

Calibrations are local metadata in `.cxt/agent-input-calibrations/`, never source
conversation mutations. Scope includes provider, model, host version, tokenizer,
window and the adapter's runtime-scope fingerprint. They contain only that hash,
maximum observed overhead and minimum observed input ceiling. Concurrent writers
merge under a lock; duplicates and delivery order cannot weaken the reserve.
Malformed records fail instead of silently becoming zero observations.

A correlated initial request can report total input (including cached input)
minus exactly measured submitted text. Keep the larger observed overhead plus
the fresh allowance; known host/framing components are not subtracted twice.
Do not infer zero from absent usage, reuse later tool-turn totals, or use
post-compaction usage as the pre-compaction input. Successful requests never
reduce the allowance automatically. Initial compaction or definite input rejection
lowers the next budget from the submitted estimated size. Feedback changes force
reselection during preparation, and changed runtime scopes cannot reuse it.

Observation recording never starts a model call. At most one retry is eligible
only for a correlated input rejection known to precede execution; a timeout,
disconnection, completed turn or compaction cannot authorize automatic replay.
Any eligible attempt still requires fresh preparation, source authorization and
runtime checks. Native event correlation and generation release are separate
integration work; this policy layer does not claim they are already wired.

Additive budget fields distinguish the accounting policy, unknown flags,
overhead allowance and observations. Legacy strict receipts keep their old wire
shape and hashes. Before preparation and launch receipts are recorded, delivery checks
that budget accounting reproduces the original request, provider, model, private
initial task and exact selected-token count within the effective limit. A saved
receipt alone cannot restore the private task reservation. Fresh Codex history
launches now use the delayed native route when the native version, existing
static catalog, launch options and exact tokenizer are supported. Refreshable
catalogs and unsupported combinations return `provider_capability_unknown`;
CXTHub never manufactures a catalog or changes native settings to enable them.
Adaptive accounting does not establish actual provider acceptance.

The preparatory [native Codex transport](NATIVE_HOST_TRANSPORT.md) has a separate
owned-process, injection and matching TUI-resume acknowledgement contract. The
isolated Codex TUI readiness path is verified without model calls. Runtime-only
receipts, the private prepared package, acknowledged injection and first-turn
completion are distinct. The actual first question triggers latest-main
selection and counting, followed by source/configuration/budget revalidation.
General dynamic-model support and actual large-model acceptance remain open;
`full=800k` remains a requested upper bound, not a proven accepted input size.

Without an explicit ref, `cxt load --output` previews the latest authorized
server `main` input, including the `200k` and `full` examples below. An explicit
`<ref>` instead selects historical archive inspection and its exact memory
revision. Both paths remain inspection artifacts without native acceptance:

```sh
cxt load --provider codex --model gpt-5.4 --context-budget 200k --output context.json
cxt load --provider codex --model gpt-5.4 --context-budget full --output context-full.json
cxt load <historical-ref> --provider codex --model gpt-5.4 --context-budget 200k --output historical-context.json
```

The artifact path counts the final rendered package
using the selected model's documented text encoding when available, preserves
selection and source provenance, and cannot be relabelled as launchable by
changing one field. A delivered managed package must be prepared from latest
authorized server `main`. Prepared and launched delivery receipts preserve a
copy of verified budget accounting when present; memory and
empty-bootstrap receipts omit it. Acceptance stays unknown without provider
evidence. Current-context mutation is never part of `load`.

### Local text accounting

The CLI embeds the vocabulary; counting needs no API key, Python, network call,
or runtime download. Its versioned counter uses ordinary-text `o200k_base` for
documented GPT-5, GPT-4o/4.1/4.5 and o1/o3/o4-mini mappings, and `cl100k_base` for
GPT-4/3.5 mappings. Mapping follows
[OpenAI tiktoken model.py](https://github.com/openai/tiktoken/blob/4e71bbe0c078468e00fefbf94b39849389f346e5/tiktoken/model.py);
it does not validate that a model exists or that a custom endpoint uses it.
Unknown/future models, deployment aliases, an omitted model and Claude keep the
inexact byte allowance (`reason: model_tokenizer_unavailable`). Claude's legacy
public tokenizer is not treated as an exact counter for current Claude models.
The Go regex engine uses Unicode 15; code points unknown to those tables and
existing assignments with category/simple-fold changes in the reference's
Unicode 16 use `reason: unicode_classification_unavailable`. Non-ASCII input
also falls back if the Go table version changes without revalidation. The
counter ID includes the Unicode-table version. This avoids labelling a newer
letter as punctuation and reporting a differing result as exact.

Every candidate and the final `Prompt()` are counted with memory, historical
JSON, escaping, notices, constraints and source pointers included. Counts of
independent chunks are never added as a substitute for counting joined text:
BPE merges can cross text boundaries. Special-token spellings in user text are
counted literally. The bounded 128-entry in-process cache holds only hashes and
counts; the counter version and encoding identify the result. Nothing about
this cache changes archive hashes or authorization checks.

The receipt's `usage.scope: text` means `exact: true` applies to these rendered
bytes under that encoding, not the provider's entire request. The supplied
initial task is counted and reserved separately for verified strict-history
preparation. Tools, system instructions, native message framing and later
interactive input still need runtime evidence.
See [OpenAI counting semantics](https://developers.openai.com/api/docs/guides/token-counting).
Local measurement does not require native-host evidence, and does not grant it.
Strict history launch remains gated on that separate evidence.

The current Go BPE implementation has quadratic work within an unbroken piece.
Inputs above 16 MiB, runs above 4 KiB, or an aggregate sum of squared run lengths
above 134,217,728 work units use the whole-text byte allowance
with `reason: tokenizer_work_limit`. They are never split into independently
counted fragments or silently truncated. Invalid UTF-8 is rejected. This bounds
individual counting work; it is not a real-time completion guarantee. An
inexact result cannot pass strict native-history delivery. Text-token equality
is tested against an independent synthetic Python tiktoken reference corpus.
Two overlapping run guards separate whitespace and letter/number classes;
punctuation with newline/slash suffixes is included in the work bound.

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
