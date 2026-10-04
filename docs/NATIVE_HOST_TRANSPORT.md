# Native host transport development

The `nativecodex` adapter is a preparatory local protocol boundary. It is not
wired into `cxt --pull codex`; public native history launch still reports
`provider_capability_unknown`. Transport success does not establish capacity,
exact token accounting or model acceptance. Interactive readiness has its own
correlated acknowledgement below; it does not enable generation.

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
question. `--no-daemon` is consumed because this is an invocation-owned private
server, not the ordinary daemon. Explicit sandbox, approval and bypass flags
are applied at `thread/start`, then inherited and checked at resume: native
remote resume rejects those flags on its command line. Replaying them literally
would prevent attachment. `--search` becomes the same final native override on
both processes. Unsupported modes remain unsupported.

This seam remains deliberately unwired to public history launch. The wrapper
still must verify configuration/source freshness at generation release and
account for the retained initial task, including native newline normalization.
Verified model/window and tokenizer evidence must feed the adaptive budget
before real history delivery. Hidden host/framing input may now use the approved
measured-reserve policy in [context input](CONTEXT_INPUT.md), with explicit
unknown labels and scoped feedback; a complete pre-send hidden-input count is
no longer a prerequisite. That change does not establish window provenance or
wire the generation relay. Codex initial prompt CRLF/CR normalization is now
applied before token reservation. A prepared
package, injection ACK, interactive connection and actual model acceptance need
separate receipts. Claude requires its own protocol adapter and evidence.

Sources: [Codex app-server](https://learn.chatgpt.com/docs/app-server),
[history injection](https://learn.chatgpt.com/docs/app-server#inject-items-into-a-thread),
[versioned native profile and launch semantics](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/cli/src/main.rs),
[WebSocket transport library](https://pkg.go.dev/github.com/coder/websocket).
