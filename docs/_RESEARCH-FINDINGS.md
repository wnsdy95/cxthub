# Session Format Research (ground truth)

> **[Frozen Document]** Preserves the historical record at the scaffolding point as the authority specification. Subsequent implementation deltas (git native hooks·ref ff-only policy·authentication/workspace/slug·stash·web UI, etc.) are not reflected. The current state is found in [`ARCHITECTURE.md`](./ARCHITECTURE.md) §6.5 and each domain document (code as the source of truth).

> This document is based on direct analysis of real Claude Code and Codex CLI session files. All design and scaffolding work treats it as the single source of truth. Do not speculate beyond the evidence recorded here.

## Environment (verified)
- Claude Code: `2.1.195`
- Codex CLI: `codex-cli 0.142.2`
- Go: `go1.26.3 darwin/arm64`  ·  Node: `v22.22.3`  ·  pnpm: `11.5.2`  ·  npm: 11.x
- Project working dir: `/Users/work/work/projects/cxthub` (not a current git repo)

## Claude Code Session Storage Format
- Location: `~/.claude/projects/<cwd-encoded>/<sessionId>.jsonl`
  - `<cwd-encoded>` = Absolute cwd string with `/` and `.` replaced by `-` (e.g., `/Users/work/foo` → `-Users-work-foo`).
  - Multiple `<sessionId>.jsonl` files possible in a directory. **Active session detection** estimates the most recently modified jsonl in the cwd-encoded directory.
- Line-based JSONL. Records `type`: `queue-operation`, `user`, `assistant` (other types like `system`, `summary` possible).
- Common metadata fields for `user`/`assistant` records:
  - `parentUuid`, `isSidechain`, `promptId`, `type`, `uuid`, `timestamp`,
    `cwd`, `sessionId`, `version`, `gitBranch`, `userType`, `entrypoint`, (`isMeta`)
  - **`gitBranch` field is embedded in the record** → Useful for automatic branch detection.
- `message` field = Anthropic message format:
  - user: `{ "role":"user", "content": <string | block[]> }`
  - assistant: `{ "model", "id", "type":"message", "role":"assistant",
    "content": block[], "stop_reason", "stop_sequence", "stop_details", "usage", "diagnostics" }`
  - content block types: `thinking`(+`signature`), `text`, `tool_use`(`{type,id,name,input}`),
    and user-side `tool_result`(`{type,tool_use_id,content}`).
  - **`thinking.signature` is a provider (Anthropic) signature/lock** — Reasoning verifiable by other providers, cannot be replayed.
- Model example: `claude-opus-4-8`.

## Codex CLI session storage format (rollout)
- Location: `~/.codex/sessions/YYYY/MM/DD/rollout-<ISO_ts>-<uuid>.jsonl`
  - File naming: timestamp + uuid. **Active session detection**: `session_meta.payload.cwd` matches current cwd and mtime is latest.
  - Index file also exists: `~/.codex/session_index.jsonl`.
- Line-by-line JSONL. All lines: `{ "timestamp", "type", "payload" }`.
- Top-level `type`: `session_meta`, `event_msg`, `response_item`, `turn_context`.
- `session_meta.payload`:
  - `{ id, timestamp, cwd, originator:"codex-tui", cli_version, source:"cli",
      thread_source, model_provider:"openai", base_instructions:{text}, ... }`
  - **No direct `gitBranch` field has been confirmed** → query the Git branch separately from the working directory.
`response_item.payload` (OpenAI Responses API item), observed `type` distribution:
  - `message`        : `{ type, role:("user"|"developer"|"assistant"), content:[{type:("input_text"|"output_text"), text}] }`
  - `function_call`  : `{ type, name, arguments(JSON string), call_id }`
  - `function_call_output` : `{ type, call_id, output }`
  `reasoning`      : `{ type, summary:[], encrypted_content }`  ← **encrypted_content is locked by the provider (OpenAI)**, cross-playback not possible
  - `custom_tool_call`        : `{ type, status, call_id, name (for example, "apply_patch"), input }`
  - `custom_tool_call_output` : `{ type, call_id, output }`
  - `web_search_call`         : `{ type, status, action:{type,query,queries} }`
`event_msg`, `turn_context` are UI/turn metadata (auxiliary info for playback).

## Hooks (Automatically Capturable)
- Both CLIs support hooks. The empirically verified Codex `~/.codex/hooks.json` format:
  ```json
  { "hooks": {
      "SessionStart":      [{ "matcher":"startup|resume",
                              "hooks":[{ "type":"command", "command":"<bin> hook --provider codex --event SessionStart", "timeout":10 }]}],
      "Stop":              [{ "hooks":[{ "type":"command", "command":"<bin> hook --provider codex --event Stop", "timeout":10 }]}],
      "UserPromptSubmit":  [{ "hooks":[{ "type":"command", "command":"<bin> hook --provider codex --event UserPromptSubmit", ... }]}]
  }}
  ```
  → A pattern for calling a single binary as a `hook` subcommand is already in use. Our tool will adopt the same pattern.
- Claude Code hooks are also of the same type (`SessionStart`/`Stop`/`SessionEnd`/`PreToolUse` etc.). Plugins register hooks in `hooks/hooks.json` or settings.

## MCP / Plugin Registration
- Claude Code Plugin: `<plugin>/.claude-plugin/plugin.json` + `.mcp.json`(MCP server stdio registration) + `commands/*.md`(slash commands) + `hooks/hooks.json`.
  - Marketplace: `~/.claude/plugins/marketplaces/`, installed list `~/.claude/plugins/installed_plugins.json`.
- Codex: `~/.codex/config.toml` `[mcp_servers.<name>]` block for MCP stdio server registration. `[plugins."..."]`, `[projects."<path>"]` sections exist.
- Conclusion: **A single Go binary** provides (a) `serve`(HTTP+web) (b) `mcp`(stdio MCP) (c) `hook`(hook handler) (d) CLI subcommands(save/list/fork/load/diff/sync) → Both CLI tools connect to each other.

## Codex Slash Commands + Native Subcommands (Empirically Verified, Official Documentation Confirmed)

### Codex Custom Prompt Slash Commands
- Creating a `~/.codex/prompts/<name>.md` file results in a `/prompts:<name>` slash command (available in both CLI and IDE).
- This is a **perfectly symmetrical** structure to Claude Code's `commands/<name>.md` → `/<name>` slash command.
- cxt Integration: Files like `integrations/codex/prompts/cxt-*.md` are exposed as `/prompts:cxt-*` slash commands.
- "Codex has no slash commands" is **incorrect**.

### Codex Built-in Slash Commands
- Provides built-in slash commands like `/init`, `/review`, `/diff`, `/memories`, `/skills`, etc.

### Codex Native Session Subcommands
- `codex resume [id|name] [--last]` — resumes an existing session by id or name (or `--last` for the most recent).
- `codex fork` — forks the current session.
- `codex archive <id|name>` / `codex unarchive <id|name>` — archives or restores a session.
- `codex delete <id|name>` — deletes a session.
- `codex apply` — applies changes to a session.
- Session identification is based on id or name (git-branch, team, cross-provider concepts are outside the Codex native scope).
- cxt's `SessionMaterializer` (codex): After `rollout-*.jsonl` synthesis, returns `resumeCmd="codex resume <id>"`.

### AGENTS-snippet.md Location
- `integrations/codex/prompts/AGENTS-snippet.md` is a memory injection snippet that merges into `AGENTS.md` **not** via a slash command. It exists separately from slash commands (`cxt-*.md`).

## Core Constraints for Cross-Compatibility (Mandatory for Design)
1. Both formats are convertible to **regular intermediate representation (CIR)** from "JSONL conversation logs" with minimal loss in both directions.
2. Retainable (high fidelity): user/assistant text, tool call names+arguments, tool results, timestamps, cwd, branch, model.
3. **Provider Lockdown (Cross-Playback Unavailable)**: Claude `thinking.signature`, Codex `reasoning.encrypted_content`.
   - Load within the same provider = replay as-is (high fidelity).
   - Cross-provider load = reasoning is **stored as metadata but inactive** (or plain summary fallback). Text+tool call transcript is reconstructed.
4. Tool name mapping required: Claude(`Bash`,`Edit`,`Read`,`Write`,…) ↔ Codex(`shell`/`exec`, `apply_patch`, `update_plan`, `web_search`,…).
5. Two load modes:
   - **full-context**: Full script restoration (high fidelity if possible).
   - **memory-form**: Inject distilled summary into provider-specific memory (Claude `CLAUDE.md`/memory, Codex `AGENTS.md`).

## Native Memory/Compression Features (Empirically Verified + Official Documentation Confirmation)
Both CLIs have their own memory/compression features, but **their scope and form are different and provider-dependent**. Our MemoryDistiller adapts these to source (suction)/sink (injection) on both sides.

| Axis | Claude Code (2.1.195) | Codex CLI (0.142.2) |
|---|---|---|
| Session compression | `/compact` + auto-compact. Result = "Conversation summary" block (original ~12% tokens), session message history persists (`isCompactSummary`/`compactMetadata` records empirically verified). Non-persistent | Self-contained context compression |
| Persistent Memory | **Auto Memory (v2.1.59+)**: `~/.claude/projects/<proj>/memory/MEMORY.md`(+topic-specific files). **repo scope**, all worktrees in the same git repo share. Loads first 200 lines/25KB automatically at startup. Stored content=**learned patterns/notes** (not raw conversation text). + CLAUDE.md 4-layer (org/user/project/local) | **memories_1.sqlite** `stage1_outputs(thread_id, raw_memory, rollout_summary, selected_for_phase2 …)` + `jobs`. **thread (session) scope**, background jobs **automatically summarize** the conversation and store→reinject. This machine is `generate_memories=false, use_memories=false` OFF |
| Auto save trigger | Claude determines necessity (not per session) | Background job |
| `#` shortcut | **Official documentation not specified** (previous mentions corrected). Memory editing via `/memory` or direct file editing | — |
| Claude's recent match for Codex memory-compression | **Auto Memory** (not a 1:1 match, stores learning patterns rather than the original conversation) | (original functionality) |

### Design Implications
- Scope is different: Claude=**repo scope**, Codex=**thread scope**. Our MemoryDigest = **branch scope + provider-neutral + team-shared** → Supersets of both.
- MemoryDistiller ports have **source adapter** (Claude `MEMORY.md`/compact-summary, Codex `rollout_summary`/`raw_memory` suction) and **sink adapter** (Claude `MEMORY.md`/`CLAUDE.md`, Codex `AGENTS.md`/memory DB injection).
- Since Codex memory can be off by default, the source strategy must always have a fallback of "suction if native, otherwise self-distilled from CIR".
