# cxt — Codex AGENTS.md Snippet

Add the content of this file to the `AGENTS.md` at the project root to have Codex recognize and utilize the cxt MCP tool.

---

## cxt — Session Snapshot and Team Context Sharing

cxt takes the current Codex session and snapshots it like a Git commit, synchronizes it with the central server, and cross-replays it with Claude Code.

### Available MCP Tools

| Tool Name | Purpose |
|---|---|
| `repo_init` | Registers the current repo and initializes the local `.cxt/` store. |
| `session_save` | Saves the current cwd session as a snapshot. The branch is automatically detected from git. |
| `session_list` | Queries the snapshot and ref list of the current repo/branch. |
| `session_fork` | Creates a new branch from the specified snapshot. The original is immutable. |
| `session_checkout` | Integrated checkout of fork+load or simple checkout. Restores after branching if `-b` is specified. |
| `session_load` | Restores a snapshot to a Codex session file (full/reconstructed/memory mode). |
| `session_diff` | Confirms the CIR event delta between two snapshots. |
| `memorize` | Distills the current session into a MemoryDigest and stores it (`memory_save` alias). |
| `memory_load` | Injects a MemoryDigest into AGENTS.md format. |
| `sync_push` | Pushes local snapshots/refs to the central server. |
| `sync_pull` | Pulls snapshots/refs from the central server. |

### Automatic Capture Actions

Once the hook is set up, cxt automatically operates on the following events:

- **SessionStart** (startup|resume): Baseline marking. No snapshot commit.
- **UserPromptSubmit**: Turn boundary marking. Used as a message hint for the next snapshot.
- **Stop**: Incremental snapshot commit after response completion (60-second debounce).

Hooks always return exit 0 and do not interfere with the session.

### Tool Invocation Example

**Session Storage**:
```
session_save({ "provider": "codex", "message": "Refactoring checkpoint before refactoring" })
```

**List Retrieval**:
```
session_list({ "repo_id": "<repoID>", "branch": "main" })
```

**Branch Forking**:
```
session_fork({ "repo_id": "<repoID>", "from_snapshot": "sha256:<hex>", "new_branch": "experiment" })
```

**Snapshot Restoration** (Codex session):
```
session_load({ "repo_id": "<repoID>", "ref": "main", "target_provider": "codex", "mode": "full" })
```

**Claude Code session cross-recovery** (reconstructed mode):
```
session_load({ "repo_id": "<repoID>", "ref": "main", "target_provider": "claude", "mode": "full" })
```

**Repo initialization**:
```
repo_init({ "cwd": "<projectDir>" })
```

**Fork + load integration (recommended)**:
```
session_checkout({ "repo_id": "<repoID>", "from": "main", "new_branch": "experiment", "target_provider": "codex", "mode": "full", "cwd": "<projectDir>" })
```

**Memory distillation and injection**:
```
memorize({ "cwd": "<projectDir>", "provider": "codex" })
memory_load({ "repo_id": "<repoID>", "ref": "main", "provider": "codex", "cwd": "<projectDir>" })
```

**Sync with Team Server**:
```
sync_push({ "repo_id": "<repoID>" })
sync_pull({ "repo_id": "<repoID>" })
```

### Important Notes

- When using `session_load` for cross-provider restoration (Codex → Claude or Claude → Codex), the fidelity becomes `reconstructed`.
  The reasoning (thinking/encrypted_content) is disabled, and only plain text summaries are retained.
- `memorize`/`memory_load` injects only distilled summaries and do not restore transcripts.
  Useful when the context window is insufficient.
- `session_checkout -b` is an integrated command for fork and load. Use `session_fork` for simple branching, and `session_load` for restoration without branching.
- The file for Codex memory injection is `AGENTS.md` (not `.agent`).
