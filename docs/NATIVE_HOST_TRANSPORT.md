# Native host transport development

The `nativecodex` adapter is a preparatory local protocol boundary. It is not
wired into `cxt --pull codex`; strict native history launch still reports
`provider_capability_unknown`. Transport success does not establish capacity,
exact token accounting, interactive attachment, or model acceptance.

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
  → create fresh thread → inject history once → acknowledge → close
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
  -run TestNativeCodexOfflineTransport -count=1 -v
```

It injects over 1 MiB of synthetic text, checks the exact provider-generated
persisted content and records that no generation request reached the fixture.
It does not read an existing account's credentials or transcript, use a real
model, or claim that the injected data fits a model window. The variable is
test-only; it does not enable native launch in the installed product.

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

This seam remains deliberately unwired to public history launch. The wrapper
still must preserve configuration through the interactive TUI, verify the
matching resume acknowledgement, and account for the retained initial task.
Verified model/window, host input/framing, tokenizer and compaction evidence
must feed the existing adaptive budget before real history delivery. A prepared
package, injection ACK, interactive connection and actual model acceptance need
separate receipts. Claude requires its own protocol adapter and evidence.

Sources: [Codex app-server](https://learn.chatgpt.com/docs/app-server),
[history injection](https://learn.chatgpt.com/docs/app-server#inject-items-into-a-thread),
[versioned native profile and launch semantics](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/cli/src/main.rs),
[WebSocket transport library](https://pkg.go.dev/github.com/coder/websocket).
