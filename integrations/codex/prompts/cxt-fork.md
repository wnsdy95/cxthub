# /prompts:cxt-fork

> This file is a Codex custom prompt. Install it at `~/.codex/prompts/cxt-fork.md` to call it using the `/prompts:cxt-fork` slash command.

Fork a new branch from the specified snapshot (or the current HEAD).

Calls the MCP tool `session_fork`. The original snapshot remains immutable, and only a new branch ref is created.

## Usage

```
/prompts:cxt-fork <new-branch> [snapshot-id]
```

- `new-branch` (required): The name of the new branch to create.
- `snapshot-id` (optional): The snapshot ID (`sha256:<hex>`) to base the new branch on. If omitted, the current HEAD snapshot will be used.

## MCP Tool Invocation

**Tool Name**: `session_fork`

**Input Example**:
```json
{
  "repo_id": "<current repo ID>",
  "from_snapshot": "sha256:<hex>",
  "new_branch": "feature-experiment"
}
```

**Example Output**:
```json
{
  "branch": "feature-experiment",
  "head": "sha256:<hex>"
}
```

## Operation

1. Identifies the current repo (git remote URL or cwd fallback).
2. Creates a new `Branch` ref from the `from_snapshot`.
3. The original branch and snapshot remain unchanged.

## Relationship to Native Codex Forks

Codex native `codex fork` forks the Codex itself session. cxt `/prompts:cxt-fork` manages a neutral provider branch layer (`.cxt/refs/`) above it, allowing cross-sharing with Claude Code.

CLI equivalent: `cxt fork`
