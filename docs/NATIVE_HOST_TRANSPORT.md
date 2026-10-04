# Native host transport development

The `nativecodex` adapter connects fresh `cxt --pull codex` launches to the
delayed first-turn path described below. Only the supported native version,
an already-configured static catalog, supported launch options and an exact
text tokenizer qualify. Ordinary refreshable catalogs still report
`provider_capability_unknown`. CXTHub does not change a catalog, model,
authentication or window setting to enable this path. Transport success and
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

**Refreshable native catalogs remain unsupported for preparation.** `model/list`
omits window metadata; `modelProvider/capabilities/read` returns feature booleans.
A debug/cache snapshot cannot prove the descriptor retained by the execution
thread. General support needs a native read/admission contract exposing that
owned thread's resolved model descriptor and revision. Public launch uses the
existing static binding and rejects unsupported configurations before starting
the TUI. It does not fall back to unverified materialized history.

## Public CLI lifecycle

`cxt --pull [--context-budget 200k|full] codex [native options] [question]`
preserves the original executable, selected directory, native model/configuration
and supported permission/terminal options. Its app-server and TUI use the same
captured environment. The default memory-only path and explicit native resume
keep their existing behavior. Claude's separate no-turn reference adapter is
not yet wired into public history launches.

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
Claude still needs public launch, interactive resume, budgeting and actual
model-acceptance integration beyond its no-turn reference adapter.

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

## Claude no-turn references

The `nativeclaude` adapter starts its own fresh Claude stream-JSON process. It
initializes the control protocol, reads `get_context_usage` with
`detail="summary"`, and can append one literal user-content reference with
`shouldQuery=false`. It never submits the user's first question, grants a
permission request, imports assistant/tool roles, or exposes arbitrary control
requests. Original roles and provenance belong inside the quoted reference.

`client_composed=true` prevents slash/path expansion of that reference. Claude
2.1.285 acknowledges this non-querying message through its ordered command
lifecycle and a successful zero-turn result. Both the fresh session UUID and
message UUID must match; token usage, API duration and turn count must be zero.
The native version does not guarantee a user-message replay for this operation.
If a replay arrives, its identity and exact text are checked separately. An
ambiguous append cannot be retried in the same session. The command receipt
explicitly leaves persistence and provider acceptance unverified. A separate
read-only `VerifyArchive` can establish exact file
readback after successful Close; this is not a power-loss/fsync durability
guarantee. Close drains and validates output through EOF; unexpected model,
permission or compaction activity, malformed frames and cancellation invalidate
the session. Native archives are preserved even after failure.

For `isSynthetic=true`, Claude 2.1.285 prefixes the stored reference with
`[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]` followed by a newline. This
48-byte provenance marker is retained. The receipt distinguishes the original
payload hash/size from the exact native content hash/size. Archive verification
compares the entire expected native text; it does not strip arbitrary prefixes
or accept a substring match. Future input budgeting must count this native
projection as well as the package text.

### Private idle-session resumption

The next transport step is deliberately private to the native adapter. An owned
session can issue an opaque, one-use resume plan only after the helper has
retired successfully and its exact saved archive has been verified. The plan
retains the original supported executable, working directory, environment,
model and option vector. Its terminal launch contains the owned absolute
`<session-uuid>.jsonl` path and no initial question or helper stream-JSON flags.
The launch rechecks the saved archive and execution identity; failed validation
or cleanup preserves the archive and cannot start a replacement process.

Preserving the option vector does not prove that mutable settings, instruction
or MCP configuration files are identical when the new process reads them.
Likewise, validation immediately before native launch is not an atomic lock on
what another process subsequently opens. These are separate executing-TUI
binding requirements; private process startup must not be promoted to public
input authorization, composer readiness or model acceptance.

The idle handoff probe uses only synthetic references and a network-denied PTY.
It must distinguish an actual composer with the loaded reference from login,
trust and first-run setup screens. The fixture never submits a question or
changes credentials/trust to make an unready screen pass. An unavailable
composer remains an explicit incomplete native verification. Terminal output is
bounded and kept out of logs. Native storage and process cleanup remain
independently testable when interactive readiness is unavailable.

The isolated Claude 2.1.285 probe on 2026-10-05 preserved the exact 1 KiB
reference and retired both processes, but observed only welcome/theme screen
markers. It did not observe the composer or loaded reference before its bounded
deadline. This opt-in readiness assertion failed; the dependent 1.5 MiB probe
was not run. No question was submitted, and no model-start or compaction marker
was observed. OS network denial prevents successful external requests; it does
not prove that native attempted none. Public Claude history delivery remains
disabled. A real initialized TUI fixture, first-question insertion/release,
executing-TUI policy binding and exact Claude accounting are still required.

The native context summary is a **local estimate**. Its model window and
compaction settings are observations of local host policy, not proof of API
capacity or an exact tokenizer. A successful append does not activate public
`cxt --pull claude` or establish 800k support. The next integration must preserve
the user's native options, attach the same saved session, account for the actual
first question and verify the model's result separately.

The optional macOS real-binary test uses a fresh private HOME/config/project,
fixed credential-free environment and an OS sandbox that denies all network
access. Before launching Claude it verifies that synthetic outside-file reads,
writes and a loopback connection receive `EPERM`. The profile allows one
resolved, root-owned ICU timezone data file required by native startup; it does
not open user preferences or keychain access. The earlier isolated-startup
timeout was this missing OS data dependency, not a remaining initialization
defect.

```sh
CXT_TEST_NATIVE_CLAUDE=/absolute/path/to/claude \
  go -C cli test ./internal/adapters/nativeclaude \
  -run TestNativeClaudeOfflineReference -count=1 -v
```

The fixture appends synthetic 1 KiB and 1.5 MiB references in separate sessions,
checks the local estimate before/after, then compares the exact native archive
after orderly shutdown. Neither the payload nor provider account details enter
test logs. All network access is denied; no actual model acceptance is claimed.

Protocol references: [Claude CLI flags](https://code.claude.com/docs/en/cli-reference)
and the versioned `@anthropic-ai/claude-agent-sdk` types. The adapter's supported
versions are explicit; a newer installed version needs compatibility verification.
