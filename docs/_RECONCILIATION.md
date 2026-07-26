# RECONCILIATION — Authority Specification (Authority Delta, R1)

> **[Frozen Document]** This authority specification is preserved as the historical record at the scaffolding point. Subsequent implementation deltas (git native hooks, ref ff-only policy, authentication/workspace/slug, stash, web UI, etc.) are not reflected. The current state is found in [`ARCHITECTURE.md`](./ARCHITECTURE.md) §6.5 and each domain document (code as the source of truth).

> This document is an **authority delta** that integrates confirmed user decisions after the SPINE phase and minor gaps (9 cases) reconciliation. All propagation agents treat this document as a single contract. If there is a conflict between this document and the existing SPINE/code, **this document takes precedence**. Build immutability: `cd backend && go build ./... && go vet ./...` passes, `cd frontend && node_modules/.bin/tsc --noEmit` passes. Maintain GREEN after all changes.

Based on: [PRIMARY-USE-CASE.md](./PRIMARY-USE-CASE.md), [DELIVERY-MODEL.md](./DELIVERY-MODEL.md), [_RESEARCH-FINDINGS.md](./_RESEARCH-FINDINGS.md), workflow coherence report.

---

## A. Command Vocabulary = Git-like (user surface). Existing session_* names are maintained within MCP.

### A.1 CLI Subcommand (`cxt <cmd>`) — User Surface
Entry points:
- `cxt serve` / `cxt mcp` / `cxt hook --provider <claude|codex> --event <Name>` (no changes)
  <!-- R2 comment: `cxt serve` was separated into `cxtd` (backend-independent binary) in R2. This document preserves the R1 history, and there is no serve subcommand in the cxt CLI after R2. HTTP server startup is exclusive to `cxtd`. -->

git command usage:
| Command | Meaning | Use Case |
|---|---|---|
| `cxt init` | Registers the current repo (`git remote origin` auto-detection, fallback to cwd). Creates local `.cxt/` store. | `InitRepo` |
| `cxt repo create <github-url>` | Explicitly registers a repo | `InitRepo` (RemoteURL specified) |
| `cxt save [-m msg]` | Commits the active session as a raw snapshot to the current branch (branch auto-detection) | `SaveSession` |
| `cxt list` / `cxt log` | Lists snapshots/refs | `ListSessions` |
| `cxt checkout <ref>` or `cxt checkout -b <new> [--from <ref>]` | **Combined fork (with `-b`) and load.** Restore context into a session for the target provider. | `CheckoutSession` |
| `cxt fork <from> --as <newBranch>` | fork only (subcommand, maintain) | `ForkSession` |
| `cxt load <ref> [--mode full\|memory] [--provider claude\|codex]` | Restore only (subcommand, maintain) | `LoadSession` |
| `cxt memorize` | Distill active session → MemoryDigest, attach to current branch | `Memorize` |
| `session_diff` (web/MCP only, CLI not implemented) | snapshot delta | `DiffSnapshots` |
| `cxt push [--no-autosave]` | Synchronize the current working directory by committing the raw snapshot and recent memory attachment as a raw+memory upload | `SyncRepo.Push` |
| `cxt pull` | Fetch from remote | `SyncRepo.Pull` |

Common flags: `checkout`/`load` have `--mode full|memory` (default full), `--provider` (default=detected current provider, otherwise cross-provider conversion).

### A.2 MCP Tools — Existing 9 + 3 Additional
Maintain existing (SPINE §7.1): `session_save, session_list, session_fork, session_load, session_diff, memory_save, memory_load, sync_push, sync_pull`.
Additional:
| Tool | Description |
|---|---|
| `repo_init` | Register current repo + `.cxt/` creation |
| `session_checkout` | Fork (+load integration) — restore context to target provider session |
| `memorize` | Alias for `memory_save` (active session distillation·attachment). In-session friendly name |

### A.3 Slash Commands (claude + codex) — 1:1 with MCP Tools
Claude: `integrations/claude-code/commands/` directory. Codex: `integrations/codex/prompts/` directory (`~/.codex/prompts/<name>.md` → `/prompts:<name>`). Both providers have slash commands and are symmetric. Expose the following (SPINE §7 9 + all additional):
`cxt-init, cxt-save, cxt-list, cxt-log, cxt-fork, cxt-checkout, cxt-load, cxt-diff, cxt-memorize, cxt-memory-load, cxt-push, cxt-pull`.
- The existing `cxt-sync.md` (push/pull integration single) is **deleted** and split into `cxt-push.md`/`cxt-pull.md` (review gap 4).
- `cxt-memorize.md` = `memorize`/`memory_save`, `cxt-memory-load.md` = `memory_load` (review gap 4: memory slash command omission resolved).
- `cxt-log.md` = `cxt-list.md` synonym. MCP tool `session_list` mapping (CLI `cxt log` = `cxt list`).
- **Codex slash = `~/.codex/prompts/cxt-*.md` (`/prompts:cxt-*`).** Codex registers `~/.codex/prompts/<name>.md` files as custom prompts for `/prompts:<name>` slash commands (CLI·IDE both). This is symmetric to Claude's `commands/*.md`. Therefore, `integrations/codex/prompts/cxt-*.md` files contain the definition of slash commands. `integrations/codex/prompts/AGENTS-snippet.md` is a memory snippet injected into `AGENTS.md` separately from slash commands. Additionally, Codex provides native subcommands `codex resume [id|name] [--last]`, `codex fork`, `codex archive|delete|unarchive <id|name>`, `codex apply`, and built-in slashes `/init`, `/review`, `/diff`, `/memories`, `/skills`.

---

## B. Memory Port = Native Ingestion + Self-Distillation Fallback (Confirmed)

Replace the single `MemoryDistiller` in `internal/ports/outbound` with the following 3 ports.

```go
// MemorySource: Absorb native memory from provider (if available). (Claude MEMORY.md / Codex rollout_summary etc.)
type MemorySource interface {
    Provider() domain.ProviderKind
    ReadNative(ctx context.Context, cwd string) (native domain.NativeMemory, found bool, err error)
}

// MemoryDistiller: Absorb native memory if available, otherwise (nil) distill from CIR. (Native-first + fallback)
type MemoryDistiller interface {
    Distill(ctx context.Context, cir domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error)
}

// MemorySink: Inject MemoryDigest into target provider native memory file. (Claude CLAUDE.md/MEMORY.md, Codex AGENTS.md)
type MemorySink interface {
    Provider() domain.ProviderKind
    Inject(ctx context.Context, digest domain.MemoryDigest, cwd string) (writtenPath string, err error)
}
```

domain value object addition:
```go
// NativeMemory: native memory created by the provider CLI (ingestion source).
type NativeMemory struct {
    Provider   ProviderKind
    Source     string            // e.g., "claude:MEMORY.md", "codex:rollout_summary"
    Text       string
    Structured map[string]string // optional
}
```

Adapter stubs (`internal/adapters`): `MemorySource`/`MemorySink` implementation stubs for claude and codex, plus a self-distillation `MemoryDistiller` stub (all `errNotImplemented`). Recommended new package `internal/adapters/memory/` (or within codec).

---

## C. Transfer Model = File backbone + MCP support / `.cxt/` store / full=native resume+fallback

### C.1 SessionMaterializer port addition (`outbound`) — full-context restoration
```go
// SessionMaterializer: materialize CIR as a target-provider session file to enable native resume.
type SessionMaterializer interface {
    Provider() domain.ProviderKind
    Materialize(ctx context.Context, cir domain.CIRDocument, cwd string) (sessionPath string, resumeCmd string, err error)
}
```
Adapter stub: claude(`~/.claude/projects/<cwd-encoded>/<newid>.jsonl` join + `resumeCmd="claude --resume <id>"`), codex(`~/.codex/sessions/.../rollout-*.jsonl` join + `resumeCmd="codex resume <id>"`). All `errNotImplemented`.

### C.2 LoadSession use-case logic (specified in the placeholder documentation comment)
- `Mode==full`: codec.Encode(cir, target) → SessionMaterializer.Materialize → ResumeCmd return. **Materialize failure automatically falls back to memory mode** (Fidelity downgrade).
- `Mode==memory`: MemorySource.ReadNative → MemoryDistiller.Distill(cir, native) → MemorySink.Inject. WrittenPath return.
- CIR is the source of truth. Add `ResumeCmd string` field to LoadOutput.

### C.3 `.cxt/` Local Store Layout (corresponds to git's .git)
SessionStore(FileStore) persists to the `.cxt/` directory of the repo root:
```
.cxt/
├── objects/            # content-addressed blob: SessionDoc(CIR) + MemoryDigest
├── refs/heads/<branch> # Branch → Snapshot.ID
├── refs/tags/<name>
├── HEAD                # Symbolic ref (current branch)
├── manifest.json       # Manifest (snapshot/ref catalog)
└── config              # remote URL, TeamIdentity
```
**Does not hijack native `.claude/`·`AGENTS.md`.** These are written to a sink only on load.

---

## D. Add inbound use-case (`internal/ports/inbound` + `internal/app` stub)

```go
type InitRepo interface {
    Init(ctx context.Context, in InitInput) (InitOutput, error)
}
type CheckoutSession interface {
    Checkout(ctx context.Context, in CheckoutInput) (CheckoutOutput, error)
}
type Memorize interface {
    Memorize(ctx context.Context, in MemorizeInput) (MemorizeOutput, error)
}

// DTO (name/type only):
// InitInput     { Cwd string; RemoteURL string }            // RemoteURL "" means auto-detect origin
// InitOutput    { RepoID string; LocalStorePath string }
//
// CheckoutInput { RepoID string; From string; NewBranch string; TargetProvider domain.ProviderKind; Mode domain.FidelityTier; Cwd string }
//                 // NewBranch != "" => fork then load(= -b). NewBranch=="" => simple load(checkout).
// CheckoutOutput{ Branch string; Head domain.ContentHash; WrittenPath string; ResumeCmd string; Fidelity domain.FidelityTier }
//
// MemorizeInput { Cwd string; Provider domain.ProviderKind }
// MemorizeOutput{ SnapshotID domain.ContentHash; MemoryHash domain.ContentHash; Attached bool }
```
`app/` directory: add stubs for `init_repo.go`, `checkout_session.go`, `memorize.go`. Inject required outbound ports in constructors (even if empty wiring). Add new subcommand/tool routing stubs in `cmd/cxt/main.go` and `adapters/delivery/cli`·`mcp`.

---

## E. Domain Enhancement

- `domain/values.go`: Add constant `ProviderUnknown ProviderKind = "unknown"` (fix schema/TS enum alignment issue).
- `domain/entities.go` `Snapshot`: Add `MemoryHash ContentHash` fields (optional, "" possible). **Snapshots should hold both raw(DocHash) and memory(MemoryHash)** (PRIMARY-USE-CASE invariant 1).
- Add to `SessionStore` port:
```go
    PutMemory(ctx context.Context, digest domain.MemoryDigest) (domain.ContentHash, error)
    GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error)
```
- Document in `RemoteSync.Push` method that it sends both raw doc and memory.

---

## F. Canonical tool_name Vocabulary (Validation Gap 1) — Declare as Source of Truth for CROSS-COMPAT §2.1

Canonical tool names: `shell, apply_patch, read_file, list_dir, grep, web_search, update_plan, mcp:<server>:<tool>, unknown:<original_name>`.
- Replace `schemas/cir.schema.json` `tool_name` description example with the following terms (`edit_file`/`write_file` removed; claude Edit/MultiEdit/Write → `apply_patch`).
- `tool_mapping.go` (already uses apply_patch) should be verified. Add a note in CROSS-COMPAT.md §2.1 at the top: "This table is the source of truth for the regular tool_name."
SPINE §5.3 also adds a regular word.

---

## G. Web REST Action Endpoints (Validation Gap 2) — SYNC-PROTOCOL + Backend HTTP + Frontend API-Routes 1:1

Maintain existing resource paths (`/api/v1/repos`, `/manifest`, `/branches`, `/snapshots`, `/docs`, `/refs`, `/memories/{snapshotId}`, `/push`, `/pull`). **Additional Action Endpoints** (for Web UI):
| Method | Path | Body | Meaning |
|---|---|---|---|
| POST | `/api/v1/repos/{repoId}/diff` | `{ left, right }` | DiffSnapshots |
| POST | `/api/v1/repos/{repoId}/fork` | `{ from, newBranch }` | ForkSession |
| POST | `/api/v1/repos/{repoId}/load` | `{ ref, mode, provider }` | LoadSession (server-side returns metadata/preview; actual file restoration is the local CLI's responsibility) |
Sync targets:
- Add the above 3 lines to `docs/SYNC-PROTOCOL.md` §2.
- Enumerate the above paths in the routing doc comment of `backend/internal/adapters/delivery/http/server.go`.
- Ensure `frontend/src/infrastructure/http/api-routes.ts` matches the above paths (`/api/v1/...`, `/memories/{snapshotId}` plural). Update old paths (`/api/repos/...`, `/memory/:id` singular) in `FRONTEND-ARCHITECTURE.md` §4.3 table.

---

## H. Document/Tree Drift Corrections (Validation Gaps 5·6·7)

- `docs/_SPINE.md` §2 Tree: Move `go.mod`/`go.sum` to `backend/go.mod` (no root go.mod). List 11 actual facet documents + the _RECONCILIATION.md in the `docs/` section. Add `manifest.schema.json` to `schemas/`. Comment out `.cxt/` (runtime-generated, gitignore). Rename capture files to actual names (`claude_capture.go`, `codex_capture.go`, `encode_cwd.go`, `coordinator.go`, `secret_scrubber.go`).
- `docs/CAPTURE.md` §6 file plan: update it to the actual filenames. Record that `debounce.go` was absorbed into `coordinator.go` (or is a later deliverable).
- SPINE §6 updates memory ports/SessionMaterializer/new use-case/domain enhancements (B·C·D·E).

---

## Validation After Application
1. `cd backend && go build ./... && go vet ./...` → exit 0
2. `cd frontend && node_modules/.bin/tsc --noEmit` → exit 0
3. Regular tool_name·MCP tool·slash command·REST path matches schema/docs/code 3rd party
4. New commands (init/checkout/memorize) exist in cli·mcp·integrations
