# Native host transport development

The `nativecodex` adapter is a preparatory local protocol boundary. It is not
wired into `cxt --pull codex`; public native history launch still reports
`provider_capability_unknown`. Transport success does not establish capacity,
exact token accounting or model acceptance. Interactive readiness has its own
correlated acknowledgement below. A separate first-turn gate now connects an
application-prepared package to native generation; it is not a capacity proof
and remains unwired to the public launch route.

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
aliases, fallback entries, invalid numbers and observed config changes fail.
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
owned thread's resolved model descriptor and revision. The public CLI supervisor
has not yet been switched from materialized-session launch to the native delayed
first-turn lifecycle; this static binding alone does not activate `--pull` or
claim actual 800k acceptance.

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
  → inject prepared history once → revalidate → forward original turn/start
  → correlated output/usage/completion → scoped input-reserve feedback
```

The private composition bridge binds the real question (including a question
first entered interactively) to the package's in-memory token reservation. It
requires a verified preparation budget, exact model/host binding, the approved
measured-reserve policy and `latest_server_main` source selection. Preparation
and validation remain application responsibilities; passing a callback or
receiving a native ACK does not supply verified model-window evidence.

Known turn parameters may only repeat the acknowledged runtime settings. Mixed
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

This seam remains deliberately unwired to public history launch. The wrapper
still must verify configuration/source freshness at generation release and
account for the retained initial task, including native newline normalization.
Verified model/window and tokenizer evidence must feed the adaptive budget
before public native history delivery. Hidden host/framing input may use the approved
measured-reserve policy in [context input](CONTEXT_INPUT.md), with explicit
unknown labels and scoped feedback; a complete pre-send hidden-input count is
no longer a prerequisite. The first-turn relay and private application bridge are implemented, but
verified model-window provenance and public runtime wiring remain outstanding. Codex initial prompt CRLF/CR normalization is now
applied before token reservation. A prepared
package, injection ACK, interactive connection and actual model acceptance need
separate receipts. Claude requires its own protocol adapter and evidence.

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
