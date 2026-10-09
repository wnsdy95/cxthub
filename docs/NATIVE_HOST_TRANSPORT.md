# Native host transport development

The `nativecodex` adapter connects fresh `cxt --pull codex` launches to the
delayed first-turn path described below. Supported stock native versions and
launch options use either an existing static catalog binding or an explicitly
estimated window from native's current model cache. CXTHub does not fork Codex
or change a catalog, model, authentication or window setting to enable this path. Transport success and
runtime readiness never establish actual model acceptance.

## Owned Codex app-server

The adapter starts one local `codex app-server` with a private temporary Unix
endpoint. Codex can publish that endpoint as a symlink to another private socket.
Before any WebSocket handshake or history is sent, CXTHub verifies the kernel
peer PID against its owned, still-running child: `LOCAL_PEERPID` on macOS and
`SO_PEERCRED` on Linux. Other operating systems are unsupported by this adapter.
No remote URL, existing daemon, or existing thread can be selected through it.

The supported sequence is:

```text
start owned process → connect owned peer → initialize → initialized
  → create fresh thread → inject history once → acknowledge
  → optional private TUI handoff → matching resume acknowledgement → close
```

Requests have correlated IDs, bounded waits, a 16 MiB encoded-message limit,
and shared per-request notification limits of 256 messages / 32 MiB. These are
transport limits, not token budgets. A protocol failure, cancellation after entering RPC or lost connection
invalidates the connection. Already-cancelled or queued lifecycle calls return
without consuming an idle session. Errors exclude native message/data and
WebSocket close reasons, which may contain private input or credentials.

A fresh thread must return exact, unambiguous identity fields and an explicit
empty history array. Missing/null history and duplicate or case-aliased identity
fields are rejected before injection.

`thread/start` also disables provider model fallback. An explicitly requested
model, approval policy, or sandbox mode must match the native response. A mismatch
invalidates the session before any history is sent. The settings hash includes
the response's permission details and other effective settings, without its
thread object. It is an observation of returned settings, not a hash of loaded
instruction contents, hidden tools, or a capacity attestation.

Only user/assistant text can be injected. System/developer instructions, opaque
reasoning and tool-call/result pairs are outside this text transport contract.
A lost injection acknowledgement is ambiguous: the server may already have
persisted it. The same session cannot retry that injection. A successful receipt
binds the thread ID, serialized-item hash, item count and UTF-8 byte count, with
provider acceptance explicitly `unverified`.

Close interrupts active reads without waiting on their operation lock. An exit
observer waits without reaping the child (kqueue on macOS, waitid with WNOWAIT on
Linux). Peer checks and signals share a lifecycle lock; the process is retired
before the sole reaper releases its PID. This avoids relying on Go's process
handle alone during the Darwin wait/signal race. Cleanup never signals a PID
found in a file or a numeric process group. Only CXTHub's temporary
endpoint directory is removed. Native session storage and the native server's
own resource cleanup remain the provider's responsibility. The owned child has
one CXTHub reaper; external child reaping or SIGCHLD auto-reaping is unsupported.

## Runtime window policy

`ResolveModelWindow` follows Codex 0.157.1's versioned configuration rules. It
separates the catalog base window, the resolved window after configuration and
maximum clamping, and the usable window after native's effective percentage.
For example, 272,000 at 95% yields 258,400 usable tokens. CXTHub applies its 80%
input policy and measured hidden-input reserve inside that usable amount.
An existing configuration increase is allowed only when the same bound catalog
explicitly supplies a maximum; native clamps the value to that maximum. Without
a maximum, configuration cannot enlarge packing beyond the base usable window.
A catalog maximum by itself never selects a larger window. Native telemetry is compared with the native resolved usable
window separately; matching telemetry never increases the prepared budget.

`StartWindowBound` supports an **already-configured static model catalog** on the
OpenAI-compatible native provider. A discovery app-server reads the effective
configuration without creating a thread. After capturing config and catalog
fingerprints, CXTHub closes discovery and starts a new execution app-server with
unchanged options and environment. It checks those fingerprints before and after
fresh-thread creation, then again through an invocation-owned binding before
injection. The exact catalog slug must match the acknowledged model. Duplicates,
aliases, fallback entries, invalid numbers and observed runtime config changes fail.
The fingerprint normalizes the pinned native version's serialized TUI defaults
and excludes only its screen-reader marker and animation preference, which the
TUI updates automatically. Types are checked; other settings and unknown keys
remain bound, including `auto_recap`, keybindings and resume-directory policy.
CXTHub does not edit the native configuration to pass this check.
CXTHub does not create, replace, or recommend manufacturing a catalog to enable
this path. Errors and receipts exclude private paths and configuration contents.

Reading only before and after **thread** creation is insufficient: the native
model manager loads its static catalog at **process startup**. An actual 0.157.1
probe reproduced a file changing from A to B while config reads and file hashes
both showed B but the native manager still used A. The discovery/execution split
covers this case. Repeated hashes are drift detection, not atomic filesystem
isolation; an ABA edit entirely between observations is not ruled out.

The capability is a checked **local packing policy**, not signed remote
entitlement or evidence of model acceptance. Feedback keys include the fresh
thread identity, so credentials need not be read and calibration is not reused
across invocations. This does not attest account identity or detect account changes. Hidden input remains explicitly unknown and uses
the approved measured reserve. Body-relative compaction thresholds are not
misrepresented as whole-input limits.

**Refreshable native catalogs use an estimated preparation policy.**
`StartWindowPolicy` selects the static or dynamic path from effective native
configuration; a bad explicit static catalog cannot silently fall back.
`WindowEstimate` reads native's model cache for the exact acknowledged model,
preserves its source, fetch timestamp, cache-writer version and selected-entry
fingerprint, and checks schema,
freshness and observed config/catalog drift. Reviewed cache writers 0.157.1 and
0.162.0 are supported independently of the executing native 0.157.1 version;
unknown formats remain unsupported. The five-minute freshness limit follows
native cache TTL. Timestamp/ETag changes and compatible cache-writer changes do not invalidate
identical selected-model evidence; receipts retain the original observation. It is never a `WindowBinding`.
Missing, malformed, stale or nonmatching model evidence remains
`provider_capability_unknown`; CXTHub does not invent a model or window.

The package records `catalog_estimate_reserve_v1`,
`estimated_for_preparation` and the catalog provenance. Preparation targets 80%
of the estimated usable window, capped by the requested 800k ceiling and known
compaction constraints, with question and hidden-input reserves deducted. An
exact local text tokenizer is used when supported. Otherwise the existing UTF-8
byte allowance is labelled `utf8_byte_allowance`, not actual model tokens.
Window confidence and text-count accuracy are independent.

A cache snapshot cannot prove the descriptor retained by native for the upcoming
request. No warmup inference or modified Codex binary is required. First-turn
receipts therefore record observed window, eligible input usage, initial
compaction, model rerouting, and whether usage was within the original and
observed 80% targets. Missing usage stays unknown. Window disagreement on this
estimated path does not terminate a productive conversation or replay its
question. Estimated receipts never feed the strict token-overhead calibration.
Transport tests do not establish real large-model acceptance.

## Public CLI lifecycle

`cxt --pull [--context-budget 200k|full] codex [native options] [question]`
preserves the original executable, selected directory, native model/configuration
and supported permission/terminal options. Its app-server and TUI use the same
captured environment. The default memory-only path and explicit native resume
keep their existing behavior. The Claude first-exchange route is described
below; it shares the supervisor but has a different native protocol.

Preparation creates an owned native thread and private endpoint, not a context
package. `runtime_prepared` and `runtime_launched` receipts contain no selected
token count, package hash or input-acceptance claim. Local code/worktree identity
is checked before launch and again when the actual question arrives. Server
main is selected at that later point, so an idle terminal does not freeze an
old shared source. A supplied argv question is normalized and passed literally;
an interactive question is counted when submitted.

The first-turn gate saves the exact private package (0600), records
`package_prepared`, injects once, revalidates source/permissions/model/window and
calibration, then durably records `injected_ready` before release. Validation
runs again after that write. Any failure prevents forwarding the model request;
the native archive may already contain the injected text, which is preserved.
`injected_ready` is not evidence of submission or acceptance. Correlated native
completion writes `first_turn_observed`; only successful, eligible completion is
marked `first_turn_completed`, without asserting history fidelity.

On a branch boundary the supervisor prepares and validates the replacement
runtime before stopping its own current child. Failed preparation preserves
that child. A restart retains budget/options and removes the previous initial
question; latest main is prepared for the new actual question. Owned endpoints
and app-server processes close on failed receipts, cancellation and child exit.
Source records and provider conversation files are never deleted by cleanup.
A local private capture binding ties the supervisor PID to the actual native
thread. Tool processes supply their native thread ID; managed capture verifies
that binding instead of borrowing an old terminal affinity. Prepared replacement
and current threads have separate entries, retired only by their own cleanup.
The transport remains monitored after the first turn. Unexpected disconnects
stop the owned lifecycle; unsuccessful cleanup blocks replacement launch.
Post-generation feedback/receipt write failures are reported separately and do
not terminate a productive conversation or replay the model request.

## Interactive readiness

After successful injection, `OpenHandoff` opens a one-client Unix WebSocket in
the same private directory (0700 directory, 0600 socket). Its upstream connection
rechecks the owned app-server's kernel peer identity. The endpoint is private to
the local user; it does not authenticate the TUI binary against other processes
running as that same user. The composition layer must launch the owned TUI.

The bridge permits initialization, selected inspection methods and exactly one
`thread/resume` of the fresh injected thread. A matching response must retain
the model, provider, canonical cwd and settings from `thread/start`. Only known
resume pagination fields are excluded from that comparison; collaboration-mode
settings are checked separately. A receipt is produced after that response has
been forwarded to the client. A loaded thread, an unrelated response, a live
process or a notification is never sufficient evidence. The receipt binds the
injection payload hash and settings hash, with provider acceptance `unverified`.
It does not attest rendered pixels or instruction-file contents.

Initialization has a 15-second deadline, at most 64 outstanding requests and a
512-frame / 32 MiB aggregate budget. Every frame remains bounded to 16 MiB.
Duplicate IDs, ambiguous envelopes, wrong-thread requests, unknown operations,
server requests and changed settings terminate the handoff. Disconnect cannot
be retried on this handoff. Closing it cancels its connections and removes its
socket; the separate owned control session remains available until it is closed.

This is still a **readiness-only** bridge. Turns, tool/approval requests, config
writes and other mutations remain blocked even after the resume ACK. The native
project must already be trusted; CXTHub neither accepts its trust dialog nor
writes that decision. `config/read` must return an explicit `web_search` mode
(`disabled`, `cached`, `indexed` or `live`). An absent mode is not guessed from
a default, since native features and managed requirements can change it. Resume
may only repeat that search setting, not introduce other configuration or
instruction overrides.

## Prepared first-turn generation

`OpenGenerationHandoff` is separate from inspection-only `OpenHandoff`. It opens
one fresh durable thread, materializes its metadata with native
`thread/section/move` to its existing null section, then verifies its ID, cwd and
empty turns through `thread/read`. This permits TUI resume without inserting a
placeholder conversation or consuming the one-shot history injection. Failed or
ambiguous persistence cannot be retried on the same session.

The flow is:

```text
fresh durable thread → metadata persistence → TUI resume
  → exact first question → application preparation and validation
  → inject prepared history once → revalidate → persist injection-ready receipt
  → revalidate → forward original turn/start
  → correlated output/usage/completion → scoped input-reserve feedback
```

The private composition bridge binds the real question (including a question
first entered interactively) to the package's in-memory token reservation. It
requires a verified preparation budget, exact model/host binding, the approved
measured-reserve policy and `latest_server_main` source selection. Preparation
and validation remain application responsibilities; passing a callback or
receiving a native ACK does not supply verified model-window evidence.

Known turn parameters may only repeat the acknowledged runtime settings.
For `serviceTier`, omission inherits while explicit null selects `default`;
null cannot change an acknowledged priority/flex or unspecified tier. A TUI
which resolves an unspecified tier to explicit default is rejected until that
resolution can be bound before thread creation. CXTHub does not force a tier. Mixed
text/image inputs, additional instructions, alternate models, changed permissions
or unrelated threads fail closed. Approval and user-input requests must belong
to the active thread/turn/item; only the client's correlated answer is relayed.
Interrupts target that turn. No automatic approval or model retry is performed.
Native ephemeral title-generation requests receive a local unsupported error;
they neither create another thread nor disconnect the original conversation.

Only the first correlated usage sample after model output, before any tool or
compaction boundary, is eligible for feedback. Native usage has no model-request
ID; tool-bearing turns can therefore remain unknown. Errors, missing telemetry,
reroutes and ambiguous completions cannot manufacture an accepted sample or
prove that execution never started. `modelContextWindow` telemetry is not a
capacity attestation. Neither conversation text nor error bodies enter feedback.
Feedback uses the immutable submitted package; a newer server main or legitimate
tool changes after release do not invalidate that measurement. A calibration
store failure is returned separately by `WaitGeneration` and does not terminate
the conversation or trigger another model request.

## Verification

Ordinary tests exercise malformed/oversized frames, interleaved and excessive
notifications, server-initiated requests, stale IDs, request cancellation,
concurrent close, ambiguous injection, wrong thread/worktree identity and a
socket symlink to another process. That last case must reject the connection
before sending an HTTP handshake and preserve the foreign socket.

An optional real-binary test uses a unique empty home/config directory and a
loopback synthetic provider which rejects all generation requests:

```sh
CXT_TEST_NATIVE_CODEX=/absolute/path/to/codex \
  go -C cli test ./internal/adapters/nativecodex \
  -run 'TestNativeCodex(OfflineTransport|TUIHandoff)' -count=1 -v
```

It injects over 1 MiB of synthetic text, checks the exact provider-generated
persisted content and records that no generation request reached the fixture.
It does not read an existing account's credentials or transcript, use a real
model, or claim that the injected data fits a model window. The variable is
test-only; it does not enable native launch in the installed product. The TUI
test additionally needs Python 3's standard-library PTY support. It creates its
own trusted synthetic project, injects over 1 MiB, starts the real CLI, observes
the matching resume response and checks that the connection survives subsequent
initialization without a model request. It does not capture terminal transcripts.

## Remaining integration requirements

The composition layer now has a launch-binding seam for Codex 0.157.1. It uses
the supervisor's existing argument parser, retains the selected cwd and first
task, forwards ordered config/feature overrides, and maps explicit model,
sandbox, approval, bypass, strict-config and web-search choices. Native config
resolution remains native; CXTHub does not parse user TOML. The initial task is
held privately for later host accounting and handoff, never sent by preparation.
Private config and prompts are omitted from JSON and routine binding formatting.

The installed binary verifies repeated config precedence, explicit model
precedence, and explicit bypass behavior in isolated fixtures with no model
turn. The optional test is `TestNativeCodexBoundLaunch` in `cli/cmd/cxt` and uses
the same `CXT_TEST_NATIVE_CODEX` opt-in as the transport test.

Unsupported launch options fail before creating the helper. In particular,
Codex 0.157.1 `--profile` selects a separate configuration layer through a
runtime loader override, but `app-server` rejects that option. Passing
`-c profile=...` is not equivalent. Profiles, images, extra writable roots,
provider pickers, hook-trust overrides and automatic-review modes need dedicated
mapping and validation before they can use this path. Existing public provider
passthrough behavior is unchanged.

The private TUI mapper preserves accepted root config/feature overrides and
terminal choices, uses the acknowledged canonical cwd, and withholds the first
question in inspection mode. The generation composition receives the actual
question from the first correlated `turn/start`. `--no-daemon` is consumed because this is an invocation-owned private
server, not the ordinary daemon. Explicit sandbox, approval and bypass flags
are applied at `thread/start`, then inherited and checked at resume: native
remote resume rejects those flags on its command line. Replaying them literally
would prevent attachment. `--search` becomes the same final native override on
both processes. Unsupported modes remain unsupported.

The public Codex history route uses this binding and the delayed supervisor.
The first question remains blocked until the runtime-launched receipt is
persisted. Configuration/source freshness, actual question reservation and the
measured reserve are checked again before generation release. Dynamic model
metadata and additional launch modes remain unsupported; normal catalog lookup
must not be mistaken for a bound runtime descriptor. Actual large-model input
acceptance and immediate compaction still require provider validation. A package,
injection ACK, TUI attachment and model completion are distinct evidence.
Claude uses its own local estimate policy and owned first exchange. Neither
provider's synthetic tests establish actual large-input model acceptance.

The native initialize user-agent's originator can be overridden by the desktop
environment (for example `Codex Desktop/0.157.1`). Compatibility checks compare
the version component; scope binding retains the entire identity and unchanged
environment. A different originator does not imply a different build version.

Sources: [Codex app-server](https://learn.chatgpt.com/docs/app-server),
[history injection](https://learn.chatgpt.com/docs/app-server#inject-items-into-a-thread),
[versioned native profile and launch semantics](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/cli/src/main.rs),
[WebSocket transport library](https://pkg.go.dev/github.com/coder/websocket).

The additional real-binary fixture `TestNativeCodexGenerationOffline` uses the
same isolated home and a credential-free loopback provider returning a canned
response. It verifies that the exact large history and question reach one
same-thread request after both validations, and that its correlated usage is
observed. It proves transport ordering and bytes only: no real model is called,
no actual capacity or 800k acceptance is claimed, and no account token is read.

The optional `TestNativeDeferredPublicCompositionReadiness` in `cli/cmd/cxt`
exercises the public composition with an existing TEST-ONLY static catalog:
idle readiness performs no cloud selection; invalid cloud data blocks the first
request; valid synthetic latest-main history/memory reach one loopback canned
request only after a durable injection receipt. The test uses an isolated dummy
local API key, never user credentials. It also verifies correlated outcome and
post-completion disconnect monitoring. This is not real-model capacity evidence.

## Claude first exchange and handoff

Fresh `cxt --pull [--context-budget 200k|full] claude [options] [question]`
uses the pinned 2.1.287 stream-JSON protocol. Preparation starts an idle helper
with the original launch settings and a fresh session ID already bound to
capture. It does not collect input or send a model request during replacement
preparation. The supervisor persists `runtime_starting` and activates the
session before invoking its owned starter. That starter collects the first
question, prepares latest-server-main input, appends the reference, checks the
budget, and releases the ordinary question once.

Native rules and hooks remain in charge of tool permissions. CXT displays
native requests and returns only the user's one-time allow/deny decision, or
answers to supported choice questions. Requests are correlated with the
observed tool invocation; cancellation withdraws the dialog and discards a late
reply. Replies use a serialized writer while callbacks run outside the protocol
reader and state lock. Unknown interaction forms stop the exchange without
implicit approval or automatic replay.

The native replay of a permission response must match the exact committed
response, including its request identity; it is not another permission grant.
On cancellation, the helper process group is stopped before stdin is closed.
This applies to all protocol writes, including initialization and reference
append, not only interactive permission replies.
EOF during a pending native permission dialog can otherwise mean "deny and
continue" and release another model request. A failed exit observation or
denied signal interrupts the writer without sending EOF and remains a cleanup
failure. The sole waiter eventually releases the input descriptor after actual
process exit.

Successful protocol EOF allows up to five seconds for native session cleanup
before forced termination. Canceled and failed operations do not use that
grace; a cancellation during normal cleanup wakes the existing shutdown owner
and escalates without a second signaler or reaper. A forced termination remains a failed close even if the child handles
the signal and exits zero; process retirement alone cannot authorize resume.

The pinned host's numeric thinking progress and subscription usage notifications
are drained without becoming response text, token accounting, permission or
completion evidence. Progress must identify the active question; account
notifications must identify the owned session after its question starts. A
successful correlated result and lifecycle completion are still required.

Completion requires correlated root/tool records and a successful native
result. Readback then verifies the exact reference, observed question, stable
assistant fields and tool results in the owned archive. The SDK emits assistant
blocks before final stop/usage accounting; readback validates the final stop
against that API round's observed tools and requires nondecreasing output usage.
An optional single `message` usage iteration must agree with the final aggregate;
multiple or different iterations remain unsupported.
Model, message identity, content, stop sequence and input accounting remain bound.
Unknown behavior fields and non-null context-management changes are rejected.
Null container/diagnostic fields and an empty input-transformation list are
inert provider metadata; their omitted archive equivalents have the same
projection. Active transformations and non-null values remain unsupported.
Native scalar block indices may contain gaps or ties; their stable merge order
must preserve the observed SDK sibling order. Missing indices, reversed content
and per-block index arrays are unsupported. This verifies replay order, not
unavailable original API indices.
The submitted question hash
and the native-expanded question hash remain distinct. The verified one-shot
resume plan starts the normal TUI with the original options and exact archive;
the supervisor owns its wait and termination. `runtime_launched` records that
TUI start, not the earlier model release. Input estimates, reference ACK,
response completion, archive persistence and TUI start are separate receipts.
Failed post-completion or owned-TUI-start receipt writes produce a fixed warning
without replaying the completed request or stopping the conversation. Pre-query
receipt writes, archive/launch validation and process cleanup remain mandatory.

Readback separately classifies bounded native environment/model announcements,
prompt snapshots and repeated token reminders at API-round boundaries. It
preserves their native rendering roles, including user-role session context.
One final prompt snapshot may enrich resolved tools, a previously absent prefix
and supported rendering flags after the last assistant. It must preserve the
earlier prompt, existing prefix/tool definitions and fold/echo policy, and become
the recorded selected leaf. Arbitrary trailing attachments, duplicate final
snapshots and a stale selected leaf are rejected. These records
retain their native ancestry and full-file identity. Reference-only queue flags
cannot appear on the real question or tool results. Native prompt instructions
and tool schemas are native-added overhead, not an exact pre-query measurement
or a permission grant; a token reminder is not a model-window descriptor.
The initial credential-organization marker and assistant timing/model hints
remain in the hash-bound original archive. They do not establish credentials,
permissions, capacity or another model request.

The same supervisor watches cancellation, fatal failure and branch boundaries
while the starter runs. Replacement preparation precedes retirement; retirement
joins a canceled starter and any child it returned before releasing another
starter. Cleanup failure remains an error. The original first question is
removed from subsequent branch launches.
During CXT-owned input, Ctrl-C cancels the exchange. After native TUI start,
the TUI receives and handles its own foreground-group interrupt while the
wrapper keeps supervising. SIGTERM retires the owned lifecycle in either phase.

See [input accounting and supported interaction limits](CONTEXT_INPUT.md).
The recorded real-provider full-budget test accepted 423,237 input tokens.
Exact 800k-token acceptance and repeated quality/compaction evaluation remain
separate; local window estimates and canned responses do not establish them.

## Shared Claude reference phase

The production Claude adapter supports the pinned 2.1.287 ordinary first-exchange
route. The retired 2.1.285 standalone reference runner, literal-only question
runner and isolated idle-process owner are no longer product entry points.
Their useful identity, admission, cancellation, archive-drift and launch-option
regressions run against the ordinary exchange and supervised handoff.

Before the actual question, the same owned process still appends exactly one
quoted reference with `shouldQuery=false`, `isSynthetic=true` and
`client_composed=true`. This phase is required: it prevents reference text from
becoming a new user request or triggering slash/path expansion. Original roles
and provenance remain inside the quoted reference.

The ordered command lifecycle and zero-turn result must match both session and
message UUIDs; usage, API duration and turn count must be zero. A replay, when
present, must match the exact native text. The native provenance prefix
`[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]` and newline remain part of
budgeting and exact archive verification. Ambiguous appends cannot be retried.
The append receipt leaves persistence and provider acceptance unverified.

After the ordinary question completes, `FirstExchange.VerifyArchive` checks the
reference, native-preprocessed question, assistant/tool records, permitted
metadata and selected parent chain. A one-use `IdleResumePlan` freezes the
original supported executable, cwd, environment, model and option vector.
`StartSupervised` rechecks exact owned archive/file identity and hands the native
TUI to the existing CLI supervisor, which exclusively owns Wait and termination.
No question is replayed; no archive is rewritten. The reference-only proof and
second isolated process owner are not retained as fallback paths.

These checks do not lock mutable configuration/instruction files against later
edits, make native pathname opening atomic, or attest power-loss durability.
A local native summary remains an estimate. A matching archive, successful
process start and actual provider acceptance are separate evidence.

Historical 2.1.285 offline probes are preserved in the development evidence and
Git history. Their welcome/theme-only result was not composer readiness and is
not a supported readiness test for the current path. Ordinary 2.1.287 regression
tests and supervised handoff tests replace those retired opt-in commands.

Protocol references: [Claude CLI flags](https://code.claude.com/docs/en/cli-reference)
and the versioned `@anthropic-ai/claude-agent-sdk` types. A newer installed native
version needs compatibility verification before this adapter accepts it.
