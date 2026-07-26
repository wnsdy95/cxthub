# cxthub — Claude Code Plugin

This directory contains assets for the **cxthub** plugin that connects Claude Code (local CLI: `cxt`).

cxt is a "Git + GitHub" for coding agent sessions. It automatically snapshots Claude Code sessions,
synchronizes with the central server, and cross-replays with Codex.

---

## File Structure

```
claude-code/
├── .claude-plugin/
│   └── plugin.json       # Plugin manifest (name/version/description)
├── .mcp.json             # cxt MCP server registration (stdio)
├── hooks/
│   └── hooks.json        # SessionStart/UserPromptSubmit/Stop/SessionEnd hooks → cxt hook
└── commands/
    ├── cxt-init.md          # /cxt-init         — register repo + initialize .cxt/
    ├── cxt-save.md          # /cxt-save         — save a session snapshot
    ├── cxt-list.md          # /cxt-list         — list snapshots and refs
    ├── cxt-fork.md          # /cxt-fork         — fork a branch without loading
    ├── cxt-checkout.md      # /cxt-checkout     — combined fork+load or plain checkout
    ├── cxt-load.md          # /cxt-load         — restore a snapshot without forking
    ├── cxt-diff.md          # /cxt-diff         — compare two snapshots
    ├── cxt-memorize.md      # /cxt-memorize     — distill active session → MemoryDigest
    ├── cxt-memory-load.md   # /cxt-memory-load  — inject a MemoryDigest
    ├── cxt-push.md          # /cxt-push         — push local state to the team server
    └── cxt-pull.md          # /cxt-pull         — pull team-server state locally
```

---

## Prerequisites

- **cxt binary** must be in your `$PATH`.
  ```sh
  # Verify
  cxt --version
  ```
- (Optional) Workspace URL connection + login:
  ```sh
  cxt remote add origin https://<host>/<username>/<workspace>
  cxt login    # or cxt login <token> (issued from Web Account Settings)
  ```

---

## Installation Method

### Method 1: Using cxt setup (Recommended)

Run from the repository root:

```sh
cxt setup  # repo root — includes agent hooks for full onboarding
```

The script performs the following steps:
1. Initializes the `.cxt` store (.git next to it, automatically excludes git status).
2. Installs 6 git hooks (commit·checkout·merge·push·ref·rewrite — fail-open, chaining with existing hooks).
3. Registers the workspace remote (one URL = server address + repo integrity).
4. Performs device login (browser approval, no token copy required — `--no-login` to omit).
5. Merges agent hooks (`cxt hook` entry added to `.claude/settings.json` — preserves existing settings).
6. Pulls team default setting bundles (`.claude`/`.agents`/`.codex`).

> MCP server installation, slash command copying, and plugin registration are **not** performed.

### Method 2: Manual Installation

**MCP Server Registration** — Add the following to the `.mcp.json` file in the project root:
```json
{
  "mcpServers": {
    "cxt": {
      "command": "cxt",
      "args": ["mcp"]
    }
  }
}
```

**Hook Registration** — Copy the `hooks/hooks.json` file to the Claude Code plugin hook path, or
merge the contents of this file into the existing hooks configuration.

**Slash Command Registration** — Copy the `commands/*.md` files to the Claude Code slash command directory.

---

## Operation Mode

### Automatic Capture (Hook)

Claude Code session events automatically trigger the cxt:

| Event | Action |
|---|---|
| `SessionStart` (startup\|resume) | Baseline marking. No commit. |
| `UserPromptSubmit` | Pull briefing injection (additionalContext) + turn boundary marking. No commit. |
| `Stop` | Incremental capture (60-second debounce). Snapshot commit. |
| `SessionEnd` | Forced flush of last state. Snapshot commit. |

Hooks always terminate within 10 seconds and return `exit 0`. Capture failures do not interfere with the session.

### Manual Commands (MCP + Slash)

Called directly within Claude Code:

```
/cxt-init
/cxt-save "checkpoint before refactoring"
/cxt-list
/cxt-fork experiment-branch
/cxt-checkout -b experiment-branch --from main
/cxt-load main --mode full
/cxt-diff sha256:aaa... sha256:bbb...
/cxt-memorize
/cxt-memory-load main
/cxt-push
/cxt-pull
```

---

## MCP Tool List (RECONCILIATION A.2 Source of Truth)

| Tool Name | Slash Command | Description |
|---|---|---|
| `repo_init` | `/cxt-init` | Register current repo + create `.cxt/` |
| `session_save` | `/cxt-save` | Save active session snapshot |
| `session_list` | `/cxt-list` | Query snapshot·ref list |
| `session_fork` | `/cxt-fork` | Branch from snapshot |
| `session_checkout` | `/cxt-checkout` | Integrated fork+load or simple checkout |
| `session_load` | `/cxt-load` | Restore snapshot to session file |
| `session_diff` | `/cxt-diff` | Delta of two snapshots CIR events |
| `memorize` | `/cxt-memorize` | Active session distillation → MemoryDigest (`memory_save` alias) |
| `memory_load` | `/cxt-memory-load` | Inject MemoryDigest into CLAUDE.md |
| `sync_push` | `/cxt-push` | Local → Central Server push |
| `sync_pull` | `/cxt-pull` | Central Server → Local pull |

---

## Troubleshooting

- `cxt: command not found` — `cxt` binary is not in `$PATH`. Retry after building/installing.
- MCP connection failure — Run `cxt mcp` directly to check the error.
- Hook not working — Verify the Claude Code hook settings path and ensure `hooks.json` is correctly registered.
