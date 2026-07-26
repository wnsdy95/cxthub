# cxthub — Codex CLI Integration

This directory contains configuration assets for **cxthub** to connect to the Codex CLI (local CLI: `cxt`).

cxt automatically snapshots Codex sessions (rollout JSONL), synchronizes with the central server, and cross-replays with Claude Code.

---

## Codex Slash Command: Custom Prompts

**Codex CLI provides `~/.codex/prompts/<name>.md` files via slash commands.**
`cxt-save.md` can be installed in `~/.codex/prompts/` to be callable via `/prompts:cxt-save`.

This corresponds to the mechanism in Claude Code where `commands/*.md` → `/cxt-save`.
It works on both CLI and IDE sides, and cxt provides 12 integrations in `integrations/codex/prompts/cxt-*.md`.

Codex native `codex fork` / `codex resume` / `codex archive` manage Codex sessions.
cxt `/prompts:cxt-fork` / `/prompts:cxt-load` add a provider-neutral branch layer on top of that,
enabling cross-team sharing between Claude Code and other providers.

## File Structure

```
codex/
├── config.snippet.toml   # [mcp_servers.cxt] block to merge into ~/.codex/config.toml
├── hooks.json            # SessionStart/Stop/UserPromptSubmit hooks → cxt hook --provider codex
├── prompts/
│   ├── AGENTS-snippet.md # AGENTS.md to add cxt tool usage instructions (for AGENTS.md injection only)
│   ├── cxt-init.md      # /prompts:cxt-init  → repo_init
│   ├── cxt-save.md      # /prompts:cxt-save  → session_save
│   ├── cxt-list.md      # /prompts:cxt-list  → session_list
│   ├── cxt-log.md       # /prompts:cxt-log   → session_list (synonym)
│   ├── cxt-fork.md      # /prompts:cxt-fork  → session_fork
│   ├── cxt-checkout.md  # /prompts:cxt-checkout → session_checkout
│   ├── cxt-load.md      # /prompts:cxt-load  → session_load
│   ├── cxt-diff.md      # /prompts:cxt-diff  → session_diff
│   ├── cxt-memorize.md  # /prompts:cxt-memorize → memorize
│   ├── cxt-memory-load.md # /prompts:cxt-memory-load → memory_load
│   ├── cxt-push.md      # /prompts:cxt-push  → sync_push
│   └── cxt-pull.md      # /prompts:cxt-pull  → sync_pull
└── README.md             # (this file)
```

---

## Prerequisites

- The **cxt binary** must be in your `$PATH`.
  ```sh
  cxt --version
  ```
- Codex CLI `0.142.2` or later must be installed.
- (Optional) Workspace URL connection + login:
  ```sh
  cxt remote add origin https://<host>/<username>/<workspace>
  cxt login    # or cxt login <token>
  ```

---

## Installation Method

### Method 1: Using cxt setup (Recommended)

Run from the repository root:

```sh
cxt setup  # Merges ~/.codex/hooks.json (preserves existing hooks) + /hooks approval guide
```

The script performs the following steps:
1. Initializes the `.cxt` store (.git next to it, automatically excludes git status).
2. Installs 6 git hooks (commit·checkout·merge·push·ref·rewrite — fail-open, chaining of existing hooks).
3. Registers the workspace remote (origin) (one URL = server address + repo integrity).
4. Performs device login (browser approval, token copy unnecessary — `--no-login` to omit).
5. Merges agent hooks into `~/.codex/hooks.json` (preserves existing hooks). **First-time** Codex execution → must approve the cxt item in `/hooks`.
6. Pulls team default setting bundles (`.claude`/`.agents`/`.codex`).

> `~/.codex/config.toml` MCP merge, `prompts/cxt-*.md` copy, and `AGENTS.md` edit are **not performed** (manual installation is referenced in method 2 below).

### Method 2: Manual Installation

**Step 1 — MCP Server Registration**: Add the following to `~/.codex/config.toml`:

```toml
[mcp_servers.cxt]
command = "cxt"
args    = ["mcp"]
```

**Step 2 — Hook Registration**: Copy `hooks.json` to `~/.codex/hooks.json` or merge it into the existing file.
If `~/.codex/hooks.json` already exists, add the cxt item to each event array.

**3rd Step — Custom Prompt Installation**: Copy the `prompts/cxt-*.md` files to `~/.codex/prompts/`.

```sh
mkdir -p ~/.codex/prompts
cp integrations/codex/prompts/cxt-*.md ~/.codex/prompts/
```

After installation, you can use slash commands like `/prompts:cxt-save` and `/prompts:cxt-fork` in a Codex session.

**4th Step — Enhancing AGENTS.md** (Optional): Add the content of `prompts/AGENTS-snippet.md` to the project root `AGENTS.md` to help Codex better utilize the cxt tool. Codex uses `AGENTS.md` instead of `.agent` files.

---

## Slash Command List (Custom Prompts)

| Slash Command | MCP Tool | Description |
|---|---|---|
| `/prompts:cxt-init` | `repo_init` | Register current repo + create `.cxt/` |
| `/prompts:cxt-save` | `session_save` | Save active session snapshot |
| `/prompts:cxt-list` | `session_list` | Query snapshot·ref list |
| `/prompts:cxt-log` | `session_list` | Query snapshot·ref list (synonym) |
| `/prompts:cxt-fork` | `session_fork` | Fork from snapshot to new branch |
| `/prompts:cxt-checkout` | `session_checkout` | Integrated fork+load or simple checkout |
| `/prompts:cxt-load` | `session_load` | Restore snapshot to session file |
| `/prompts:cxt-diff` | `session_diff` | Delta of two snapshots CIR events |
| `/prompts:cxt-memorize` | `memorize` | Active session distillation → MemoryDigest (`memory_save` alias) |
| `/prompts:cxt-memory-load` | `memory_load` | Inject MemoryDigest into AGENTS.md |
| `/prompts:cxt-push` | `sync_push` | Local → Central server push |
| `/prompts:cxt-pull` | `sync_pull` | Central server → Local pull |

Claude Code response: `integrations/claude-code/commands/cxt-*.md` → `/cxt-save` etc.

---

## Operation Mode

### Automatic Capture (Hook)

Codex session events automatically call cxt:

| Event | Action |
|---|---|
| `SessionStart` (startup\|resume) | Baseline marking. No commit. |
| `UserPromptSubmit` | Turn boundary marking. Next snapshot message hint. No commit. |
| `Stop` | Incremental capture (60-second debounce). Snapshot commit. |

Hooks always terminate within 10 seconds and return `exit 0`.

**Warning**: Codex does not have a `SessionEnd` event. The last `Stop` serves as the final commit.

### Active Session Detection

Codex session files are located at `~/.codex/sessions/YYYY/MM/DD/rollout-<ISO_ts>-<uuid>.jsonl`.
cxt detects the most recent file with a matching `payload.cwd` from the first `session_meta` record of each file and activates it as the current session.

### Using the MCP Tools

In the Codex session, you can directly call the cxt MCP tool:

```
# Repo Initialization
repo_init({ "cwd": "<projectDir>" })

# Session Save
session_save({ "provider": "codex", "message": "Checkpoint before work" })

# List Snapshots
session_list({ "repo_id": "<repoID>", "branch": "main" })

# Fork + Load Integration (Recommended)
session_checkout({ "repo_id": "<repoID>", "from": "main", "new_branch": "experiment", "target_provider": "codex", "mode": "full", "cwd": "<projectDir>" })

# Cross-play with Claude Code Session
session_load({ "repo_id": "<repoID>", "ref": "main", "target_provider": "claude", "mode": "full" })

# Memory Distillation and Injection of AGENTS.md
memorize({ "cwd": "<projectDir>", "provider": "codex" })
memory_load({ "repo_id": "<repoID>", "ref": "main", "provider": "codex", "cwd": "<projectDir>" })

# Team Server Synchronization
sync_push({ "repo_id": "<repoID>" })
sync_pull({ "repo_id": "<repoID>" })
```

---

## MCP Tool List (compatibility rules Source of Truth)

| Tool Name | Description |
|---|---|
| `repo_init` | Register current repo + create `.cxt/` |
| `session_save` | Save active session snapshot |
| `session_list` | Query snapshot·ref list |
| `session_fork` | Fork new branch from snapshot |
| `session_checkout` | Fork+load integration or simple checkout |
| `session_load` | Restore snapshot to session file |
| `session_diff` | Delta of CIR events between two snapshots |
| `memorize` | Active session distillation → MemoryDigest (`memory_save` alias) |
| `memory_load` | Inject MemoryDigest into AGENTS.md |
| `sync_push` | Local → Central Server push |
| `sync_pull` | Central Server → Local pull |

---

## Problem Resolution

- `cxt: command not found` — Build the cxt binary and add it to your PATH.
- MCP connection failure — Run `cxt mcp` directly to check the error.
- Hook not working — Verify that `~/.codex/hooks.json` is in the correct format.
- Session detection failure — Check if there is a rollout file corresponding to the current cwd in `~/.codex/sessions/`.
- Slash command not showing — Verify that the `~/.codex/prompts/cxt-*.md` files are installed (`ls ~/.codex/prompts/`).
